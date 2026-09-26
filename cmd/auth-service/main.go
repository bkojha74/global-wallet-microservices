package main

import (
	"context"
	"fmt"
	"log"
	"net"
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
	mongoURI         string
	environment      string
	authProviderType string
}

func parseAuthConfig() authConfig {
	port := os.Getenv("AUTH_SERVICE_PORT")
	if port == "" {
		port = "50054"
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
		mongoURI:         mongoURI,
		environment:      environment,
		authProviderType: authProviderType,
	}
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

	// ── Graceful Shutdown ────────────────────────────────────────────────────
	<-ctx.Done()
	log.Println("[AUTH-SERVICE] Context cancelled, shutting down...")

	healthSvc.SetServingStatus("auth.v1.AuthService", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpcServer.GracefulStop()
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
			log.Println("[AUTH-SERVICE] Keycloak unreachable at startup; falling back to local provider")
			return local
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oidcClient, err := NewOIDCClient(ctx, issuerURL, clientID, clientSecret, explicitJWKS)
	if err != nil {
		log.Printf("[AUTH-SERVICE] WARNING: OIDC client init failed for %s: %v", issuerURL, err)
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

	return NewKeycloakProvider(oidcClient, jwksCache, clientID, issuerURL), err
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

// seedDefaultAdmin creates a default admin user on first startup if no users exist.
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
		adminPass = "change-me-in-production"
		log.Println("[SECURITY-WARNING] ADMIN_PASSWORD not set — using insecure default!")
	}

	_, err := store.FindByUsername(ctx, adminUser)
	if err == nil {
		return
	}

	adminScopes := []string{
		auth.ScopeClusterAdmin,
		auth.ScopeLedgerAudit,
		auth.ScopeWalletTransfer,
		auth.ScopeWalletRead,
	}
	if err := store.CreateUser(ctx, adminUser, "", adminPass, []string{auth.RoleAdmin}, adminScopes); err != nil {
		log.Printf("[AUTH-SERVICE] Admin seed failed: %v", err)
		return
	}
	log.Printf("[AUTH-SERVICE] Default admin user seeded: username=%s", adminUser)
}
