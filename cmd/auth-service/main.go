package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	authv1 "wallet-system/proto/auth"
)

const authServiceName = "auth-service"

type authConfig struct {
	port             string
	metricsPort      string
	mongoURI         string
	environment      string
	authProviderType string
}

func parseAuthConfig() authConfig {
	port := os.Getenv("AUTH_SERVICE_PORT")
	if port == "" {
		port = "50054"
	}
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9095"
	}
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
	}
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "local"
	}
	authProviderType := strings.ToLower(os.Getenv("AUTH_PROVIDER"))
	if authProviderType == "" {
		if os.Getenv("KEYCLOAK_URL") != "" {
			authProviderType = "hybrid"
		} else {
			authProviderType = "local"
		}
	}
	return authConfig{
		port:             port,
		metricsPort:      metricsPort,
		mongoURI:         mongoURI,
		environment:      environment,
		authProviderType: authProviderType,
	}
}

func startAuthMetricsServer(metricsPort string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", observability.DefaultMetrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"SERVING","service":"auth-service"}`))
	})
	server := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[AUTH-SERVICE] Management metrics server listening on :%s/metrics", metricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[AUTH-SERVICE] Metrics server error: %v", err)
		}
	}()
	return server
}

func initMongoDB(uri string) (*mongo.Client, *mongo.Database, error) {
	mongoCtx, mongoCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer mongoCancel()
	mongoClient, err := db.ConnectWithRetry(mongoCtx, uri, 2)
	if err != nil {
		return nil, nil, fmt.Errorf("[AUTH-SERVICE] MongoDB connection failed: %w", err)
	}
	return mongoClient, mongoClient.Database("auth_db"), nil
}

func runAuthServer(ctx context.Context) error {
	cfg := parseAuthConfig()
	log.Printf("[AUTH-SERVICE] Starting on port %s (environment=%s, provider_mode=%s)", cfg.port, cfg.environment, cfg.authProviderType)

	// ── MongoDB ──────────────────────────────────────────────────────────────
	var mongoClient *mongo.Client
	var authDB *mongo.Database
	if os.Getenv("TEST_MOCK_DB") == "true" {
		log.Println("[AUTH-SERVICE] TEST_MOCK_DB=true — running in mock DB mode")
	} else {
		var err error
		mongoClient, authDB, err = initMongoDB(cfg.mongoURI)
		if err != nil {
			return err
		}
		defer func() { _ = mongoClient.Disconnect(context.Background()) }()
		log.Println("[AUTH-SERVICE] MongoDB connected (auth_db)")
	}

	// ── User Store & Revocation Store ────────────────────────────────────────
	userStore, err := NewUserStore(authDB)
	if err != nil {
		return fmt.Errorf("[AUTH-SERVICE] UserStore init failed: %w", err)
	}

	revStore, err := NewRevocationStore(authDB)
	if err != nil {
		return fmt.Errorf("[AUTH-SERVICE] RevocationStore init failed: %w", err)
	}

	// ── Token Engine ─────────────────────────────────────────────────────────
	engine, err := buildTokenEngine()
	if err != nil {
		return fmt.Errorf("[AUTH-SERVICE] TokenEngine init failed: %w", err)
	}

	// ── Seed default admin user if none exists ───────────────────────────────
	seedDefaultAdmin(ctx, userStore)

	// ── Identity Provider Assembly ───────────────────────────────────────────
	localProvider := NewLocalProvider(userStore, engine, revStore)
	activeProvider := setupIdentityProvider(cfg.authProviderType, localProvider)

	// ── Observability ────────────────────────────────────────────────────────
	logger := observability.LoggerFromEnvironment(authServiceName, cfg.environment, "", os.Stdout)
	_ = logger

	shutdownTracer, err := observability.InitTracer(authServiceName)
	if err == nil && shutdownTracer != nil {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	// ── gRPC Server ──────────────────────────────────────────────────────────
	// #nosec G102 -- microservice container listens on configured port across container network interfaces
	lis, err := net.Listen("tcp", ":"+cfg.port)
	if err != nil {
		return fmt.Errorf("[AUTH-SERVICE] Failed to listen on port %s: %w", cfg.port, err)
	}

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(observability.UnaryServerTraceInterceptor(authServiceName)),
	)

	svc := &authServer{
		provider: activeProvider,
		engine:   engine,
		store:    userStore,
		revStore: revStore,
	}
	authv1.RegisterAuthServiceServer(grpcServer, svc)

	// Standard gRPC health protocol
	healthSvc := health.NewServer()
	healthSvc.SetServingStatus("auth.v1.AuthService", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthSvc)

	go func() {
		log.Printf("[AUTH-SERVICE] gRPC server listening on :%s", cfg.port)
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			log.Printf("[AUTH-SERVICE] gRPC serve error: %v", err)
		}
	}()

	metricsServer := startAuthMetricsServer(cfg.metricsPort)

	// ── Graceful Shutdown ────────────────────────────────────────────────────
	<-ctx.Done()
	log.Println("[AUTH-SERVICE] Context cancelled, shutting down...")

	healthSvc.SetServingStatus("auth.v1.AuthService", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpcServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[AUTH-SERVICE] Metrics server shutdown error: %v", err)
	}

	log.Println("[AUTH-SERVICE] Shutdown complete.")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runAuthServer(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}

// setupIdentityProvider constructs the requested provider hierarchy (local, keycloak, or hybrid).
func setupIdentityProvider(mode string, local *LocalProvider) IdentityProvider {
	keycloakURL := os.Getenv("KEYCLOAK_URL")
	realm := os.Getenv("KEYCLOAK_REALM")
	if realm == "" {
		realm = "wallet-realm"
	}
	clientID := os.Getenv("KEYCLOAK_CLIENT_ID")
	if clientID == "" {
		clientID = "wallet-api"
	}
	clientSecret := os.Getenv("KEYCLOAK_CLIENT_SECRET")
	explicitJWKS := os.Getenv("KEYCLOAK_JWKS_URL")

	if keycloakURL == "" && explicitJWKS == "" {
		if mode == "keycloak" {
			log.Printf("[AUTH-SERVICE] WARNING: AUTH_PROVIDER=keycloak requested but KEYCLOAK_URL is empty! Falling back to local.")
		}
		log.Println("[AUTH-SERVICE] Provider: local (MongoDB + internal TokenEngine)")
		return local
	}

	issuerURL := keycloakURL
	if !strings.Contains(issuerURL, "/realms/") {
		issuerURL = fmt.Sprintf("%s/realms/%s", strings.TrimRight(keycloakURL, "/"), realm)
	}

	kcProvider, err := initKeycloakProvider(mode, issuerURL, clientID, clientSecret, explicitJWKS)
	if err != nil {
		if mode == "keycloak" {
			log.Println("[AUTH-SERVICE] Operating in degraded Keycloak mode (will retry on incoming requests)")
		} else {
			log.Printf("[AUTH-SERVICE] Keycloak temporarily unreachable at startup (%v); continuing in hybrid fallback mode", err)
		}
	}

	switch mode {
	case "keycloak":
		log.Printf("[AUTH-SERVICE] Provider: Keycloak (issuer=%s, clientID=%s)", issuerURL, clientID)
		return kcProvider
	case "hybrid":
		log.Printf("[AUTH-SERVICE] Provider: Hybrid (Keycloak issuer=%s + Local MongoDB fallback)", issuerURL)
		return NewHybridProvider(local, kcProvider)
	default:
		log.Println("[AUTH-SERVICE] Provider: local (MongoDB + internal TokenEngine)")
		return local
	}
}

func initKeycloakProvider(mode, issuerURL, clientID, clientSecret, explicitJWKS string) (*KeycloakProvider, error) {
	var oidcClient *OIDCClient
	var err error
	for attempt := 1; attempt <= 10; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		oidcClient, err = NewOIDCClient(ctx, issuerURL, clientID, clientSecret, explicitJWKS)
		cancel()
		if err == nil {
			break
		}
		if attempt < 10 {
			time.Sleep(1 * time.Second)
		}
	}
	if err != nil {
		log.Printf("[AUTH-SERVICE] WARNING: OIDC client init failed for %s after retries: %v", issuerURL, err)
	}

	jwksURL := explicitJWKS
	if jwksURL == "" && oidcClient != nil {
		jwksURL = oidcClient.JwksURI()
	}
	if jwksURL == "" {
		jwksURL = fmt.Sprintf("%s/protocol/openid-connect/certs", strings.TrimRight(issuerURL, "/"))
	}

	jwksCache := NewJWKSCache(jwksURL, 1*time.Hour)
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer bgCancel()
		if refreshErr := jwksCache.Refresh(bgCtx); refreshErr != nil {
			log.Printf("[AUTH-SERVICE] Initial JWKS fetch notice: %v (keys will be fetched on first token)", refreshErr)
		}
	}()

	provider := NewKeycloakProvider(oidcClient, jwksCache, clientID, issuerURL)
	provider.SetConfig(clientSecret, explicitJWKS)
	// Start background self-healing poller (pings every 5 seconds, auto-reconnects when Keycloak/OIDC recovers)
	provider.StartHealthPoller(5 * time.Second)

	return provider, err
}

// buildTokenEngine configures the internal token signing engine based on environment variables.
func buildTokenEngine() (*TokenEngine, error) {
	privateKeyPath := os.Getenv("AUTH_PRIVATE_KEY_PATH")
	publicKeyPath := os.Getenv("AUTH_PUBLIC_KEY_PATH")

	if privateKeyPath != "" && publicKeyPath != "" {
		privKey, err := auth.LoadRSAPrivateKey(privateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load private key %s: %w", privateKeyPath, err)
		}
		pubKey, err := auth.LoadRSAPublicKey(publicKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load public key %s: %w", publicKeyPath, err)
		}
		keyID := os.Getenv("AUTH_KEY_ID")
		if keyID == "" {
			keyID = "default"
		}
		log.Println("[AUTH-SERVICE] Internal Token engine: RS256 (asymmetric)")
		return NewRS256Engine(privKey, pubKey, keyID), nil
	}

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "development-wallet-insecure-secret-key-change-in-prod"
		log.Println("[SECURITY-WARNING] JWT_SECRET not set; using insecure development default!")
	}
	log.Println("[AUTH-SERVICE] Internal Token engine: HS256 (symmetric — set AUTH_PRIVATE_KEY_PATH for RS256)")
	return NewHS256Engine(secret), nil
}

// seedDefaultAdmin creates default admin and demo user accounts on first startup if they don't exist.
func seedDefaultAdmin(ctx context.Context, store *UserStore) {
	if store == nil || store.db == nil {
		return
	}
	adminUser := os.Getenv("ADMIN_USERNAME")
	adminPass := os.Getenv("ADMIN_PASSWORD")
	if adminUser == "" {
		adminUser = "admin"
	}
	if adminPass == "" {
		adminPass = "admin123"
	}

	if _, err := store.FindByUsername(ctx, adminUser); err != nil {
		adminScopes := []string{
			auth.ScopeClusterAdmin,
			auth.ScopeLedgerAudit,
			auth.ScopeWalletTransfer,
			auth.ScopeWalletRead,
		}
		if err := store.CreateUser(ctx, adminUser, "", adminPass, []string{auth.RoleAdmin}, adminScopes); err != nil {
			log.Printf("[AUTH-SERVICE] Admin seed failed: %v", err)
		} else {
			log.Printf("[AUTH-SERVICE] Default admin user seeded: username=%s", adminUser)
		}
	}

	// Seed demo users for local fallback resilience
	demoUsers := []struct {
		username string
		password string
		roles    []string
		scopes   []string
	}{
		{
			username: "alice",
			password: "password123",
			roles:    []string{auth.RoleUser},
			scopes:   []string{auth.ScopeWalletTransfer, auth.ScopeWalletRead},
		},
		{
			username: "bob",
			password: "password123",
			roles:    []string{auth.RoleUser},
			scopes:   []string{auth.ScopeWalletTransfer, auth.ScopeWalletRead},
		},
	}
	for _, du := range demoUsers {
		if _, err := store.FindByUsername(ctx, du.username); err != nil {
			if err := store.CreateUser(ctx, du.username, du.username+"@wallet.local", du.password, du.roles, du.scopes); err == nil {
				log.Printf("[AUTH-SERVICE] Demo user seeded in local store: %s", du.username)
			}
		}
	}
}
