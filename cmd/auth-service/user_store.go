package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"
)

// userStatus represents the lifecycle state of a user account.
const (
	UserStatusActive    = "ACTIVE"
	UserStatusSuspended = "SUSPENDED"
	UserStatusLocked    = "LOCKED"
)

// maxFailedLogins is the number of consecutive failures before account lockout.
const maxFailedLogins = 5

// UserRecord mirrors the MongoDB document stored in auth_db.users.
type UserRecord struct {
	ID               primitive.ObjectID `bson:"_id,omitempty"`
	Username         string             `bson:"username"`
	Email            string             `bson:"email,omitempty"`
	PasswordHash     string             `bson:"password_hash"`
	Roles            []string           `bson:"roles"`
	Scopes           []string           `bson:"scopes"`
	WalletIDs        []string           `bson:"wallet_ids,omitempty"` // for IDOR/ABAC ownership checks
	Status           string             `bson:"status"`
	FailedLoginCount int                `bson:"failed_login_count"`
	LastLoginAt      *time.Time         `bson:"last_login_at,omitempty"`
	CreatedAt        time.Time          `bson:"created_at"`
	UpdatedAt        time.Time          `bson:"updated_at"`
}

// RefreshTokenRecord represents a stored refresh token in auth_db.refresh_tokens.
type RefreshTokenRecord struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	Token     string             `bson:"token"`   // opaque random string
	Subject   string             `bson:"subject"` // username
	Roles     []string           `bson:"roles"`
	Scopes    []string           `bson:"scopes"`
	IssuedAt  time.Time          `bson:"issued_at"`
	ExpiresAt time.Time          `bson:"expires_at"` // TTL index target
	Revoked   bool               `bson:"revoked"`
}

// UserStore provides data access for user authentication and session management.
type UserStore struct {
	db *mongo.Database
}

// NewUserStore creates a UserStore backed by the given MongoDB database.
// It ensures required indexes exist on startup.
func NewUserStore(db *mongo.Database) (*UserStore, error) {
	s := &UserStore{db: db}
	if db == nil {
		return s, nil
	}
	if err := s.ensureIndexes(context.Background()); err != nil {
		return nil, fmt.Errorf("user_store: index setup failed: %w", err)
	}
	return s, nil
}

// ensureIndexes creates unique and TTL indexes required for correct operation.
func (s *UserStore) ensureIndexes(ctx context.Context) error {
	// Unique index on username
	_, err := s.users().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{bson.E{Key: "username", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("idx_username_unique"),
	})
	if err != nil && !isDuplicateKeyError(err) {
		return fmt.Errorf("users unique username index: %w", err)
	}

	// TTL index on refresh_tokens.expires_at (auto-delete expired tokens)
	_, err = s.refreshTokens().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{bson.E{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("idx_refresh_ttl"),
	})
	if err != nil && !isDuplicateKeyError(err) {
		return fmt.Errorf("refresh_tokens TTL index: %w", err)
	}

	// Index on refresh token string for fast lookup
	_, err = s.refreshTokens().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{bson.E{Key: "token", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("idx_refresh_token_unique"),
	})
	if err != nil && !isDuplicateKeyError(err) {
		return fmt.Errorf("refresh_tokens token index: %w", err)
	}

	log.Println("[AUTH-STORE] Indexes verified")
	return nil
}

// Authenticate verifies username+password against the stored bcrypt hash.
// It manages the failed login counter and account lockout automatically.
// Returns the user record on success.
func (s *UserStore) Authenticate(ctx context.Context, username, password string) (*UserRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user store unavailable")
	}
	user, err := s.FindByUsername(ctx, username)
	if err != nil {
		return nil, errors.New("invalid credentials")
	}

	if user.Status == UserStatusLocked {
		return nil, errors.New("account locked due to too many failed attempts")
	}
	if user.Status == UserStatusSuspended {
		return nil, errors.New("account suspended")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		// Increment failure counter and lock if threshold exceeded
		newCount := user.FailedLoginCount + 1
		update := bson.M{"$set": bson.M{
			"failed_login_count": newCount,
			"updated_at":         time.Now().UTC(),
		}}
		if newCount >= maxFailedLogins {
			update["$set"].(bson.M)["status"] = UserStatusLocked
			log.Printf("[AUTH-STORE] Account locked after %d failed attempts: %s", newCount, username)
		}
		if col := s.users(); col != nil {
			_, _ = col.UpdateOne(ctx, bson.M{"username": username}, update)
		}
		return nil, errors.New("invalid credentials")
	}

	// Successful login — reset failure counter
	now := time.Now().UTC()
	if col := s.users(); col != nil {
		_, _ = col.UpdateOne(ctx, bson.M{"username": username}, bson.M{"$set": bson.M{
			"failed_login_count": 0,
			"last_login_at":      now,
			"updated_at":         now,
		}})
	}

	return user, nil
}

// FindByUsername returns a user record by username.
func (s *UserStore) FindByUsername(ctx context.Context, username string) (*UserRecord, error) {
	col := s.users()
	if col == nil {
		return nil, errors.New("user_store: database unavailable")
	}
	var u UserRecord
	err := col.FindOne(ctx, bson.M{"username": username}).Decode(&u)
	if err != nil {
		return nil, fmt.Errorf("user_store: user not found: %w", err)
	}
	return &u, nil
}

// CreateUser inserts a new user with a bcrypt-hashed password.
// Intended for admin provisioning or a registration endpoint.
func (s *UserStore) CreateUser(ctx context.Context, username, email, password string, roles, scopes []string) error {
	col := s.users()
	if col == nil {
		return errors.New("user_store: database unavailable")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("user_store: bcrypt hash: %w", err)
	}
	now := time.Now().UTC()
	user := UserRecord{
		ID:           primitive.NewObjectID(),
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		Roles:        roles,
		Scopes:       scopes,
		Status:       UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err = s.users().InsertOne(ctx, user)
	return err
}

// StoreRefreshToken persists a new refresh token linked to the given subject.
func (s *UserStore) StoreRefreshToken(ctx context.Context, token, subject string, roles, scopes []string, ttl time.Duration) error {
	col := s.refreshTokens()
	if col == nil {
		return errors.New("user_store: database unavailable")
	}
	now := time.Now().UTC()
	rec := RefreshTokenRecord{
		ID:        primitive.NewObjectID(),
		Token:     token,
		Subject:   subject,
		Roles:     roles,
		Scopes:    scopes,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
		Revoked:   false,
	}
	_, err := col.InsertOne(ctx, rec)
	return err
}

// FindRefreshToken retrieves an active (non-revoked, non-expired) refresh token record.
func (s *UserStore) FindRefreshToken(ctx context.Context, token string) (*RefreshTokenRecord, error) {
	col := s.refreshTokens()
	if col == nil {
		return nil, errors.New("user_store: database unavailable")
	}
	var rec RefreshTokenRecord
	filter := bson.M{
		"token":      token,
		"revoked":    false,
		"expires_at": bson.M{"$gt": time.Now().UTC()},
	}
	err := col.FindOne(ctx, filter).Decode(&rec)
	if err != nil {
		return nil, fmt.Errorf("user_store: refresh token not found or expired: %w", err)
	}
	return &rec, nil
}

// RevokeRefreshToken marks a refresh token as revoked.
func (s *UserStore) RevokeRefreshToken(ctx context.Context, token string) error {
	col := s.refreshTokens()
	if col == nil {
		return errors.New("user_store: database unavailable")
	}
	_, err := col.UpdateOne(
		ctx,
		bson.M{"token": token},
		bson.M{"$set": bson.M{"revoked": true}},
	)
	return err
}

// OwnsWallet checks if the given subject is linked to the walletID (ABAC ownership check).
func (s *UserStore) OwnsWallet(ctx context.Context, subject, walletID string) (bool, error) {
	col := s.users()
	if col == nil {
		return true, nil
	}
	count, err := col.CountDocuments(ctx, bson.M{
		"username":   subject,
		"wallet_ids": walletID,
	})
	return count > 0, err
}

// AddWalletToUser links a wallet ID to a user account (called on wallet creation).
func (s *UserStore) AddWalletToUser(ctx context.Context, username, walletID string) error {
	col := s.users()
	if col == nil {
		return errors.New("user_store: database unavailable")
	}
	_, err := col.UpdateOne(
		ctx,
		bson.M{"username": username},
		bson.M{
			"$addToSet": bson.M{"wallet_ids": walletID},
			"$set":      bson.M{"updated_at": time.Now().UTC()},
		},
	)
	return err
}

// DefaultScopesForRoles returns the standard scopes granted to a set of roles.
func DefaultScopesForRoles(roles []string) []string {
	for _, r := range roles {
		if strings.EqualFold(r, "admin") {
			return []string{"cluster:admin", "ledger:audit", "wallet:transfer", "wallet:read"}
		}
	}
	return []string{"wallet:transfer", "wallet:read"}
}

func (s *UserStore) users() *mongo.Collection {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Collection("users")
}

func (s *UserStore) refreshTokens() *mongo.Collection {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Collection("refresh_tokens")
}

// isDuplicateKeyError checks for MongoDB duplicate-key errors (code 11000).
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "11000")
}
