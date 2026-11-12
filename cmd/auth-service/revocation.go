package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RevokedTokenRecord represents an access token that has been explicitly revoked
// (e.g., by logout or an admin action). Stored in auth_db.revoked_tokens with
// a TTL index that auto-removes entries once the original token would have expired.
type RevokedTokenRecord struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	TokenID   string             `bson:"token_id"` // JWT "jti" claim
	Subject   string             `bson:"subject"`
	Reason    string             `bson:"reason"`
	RevokedAt time.Time          `bson:"revoked_at"`
	ExpiresAt time.Time          `bson:"expires_at"` // TTL index; matches original token expiry
}

// RevocationStore manages the token blocklist in MongoDB.
// Access tokens are stateless JWTs, so revocation requires checking this list on each validation.
// The local claim cache in the API Gateway is also invalidated when a token is revoked.
type RevocationStore struct {
	db *mongo.Database
}

// NewRevocationStore creates a RevocationStore and ensures the TTL index exists.
func NewRevocationStore(db *mongo.Database) (*RevocationStore, error) {
	s := &RevocationStore{db: db}
	if db == nil {
		return s, nil
	}
	if err := s.ensureIndexes(context.Background()); err != nil {
		return nil, fmt.Errorf("revocation_store: index setup failed: %w", err)
	}
	return s, nil
}

// ensureIndexes creates the TTL index on expires_at and a unique index on token_id.
func (s *RevocationStore) ensureIndexes(ctx context.Context) error {
	// TTL index: MongoDB auto-removes documents after their expires_at
	_, err := s.col().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{bson.E{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("idx_revoked_ttl"),
	})
	if err != nil && !strings.Contains(err.Error(), "11000") {
		return fmt.Errorf("revoked_tokens TTL index: %w", err)
	}

	// Unique index on token_id to prevent duplicate revocation entries
	_, err = s.col().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{bson.E{Key: "token_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetSparse(true).SetName("idx_token_id_unique"),
	})
	if err != nil && !strings.Contains(err.Error(), "11000") {
		return fmt.Errorf("revoked_tokens token_id index: %w", err)
	}

	// Indexes for revoked_subjects (REQ-SEC-03: Keycloak Session Revocation Sync)
	if s.subjectsCol() != nil {
		_, _ = s.subjectsCol().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{bson.E{Key: "expires_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0).SetName("idx_revoked_subjects_ttl"),
		})
		_, _ = s.subjectsCol().Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{bson.E{Key: "subject", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("idx_revoked_subject_unique"),
		})
	}

	log.Println("[REVOCATION-STORE] Indexes verified")
	return nil
}

// RevokedSubjectRecord represents subject-level session revocation (e.g., Keycloak backchannel logout).
// Any token issued prior to RevokedBefore is invalidated.
type RevokedSubjectRecord struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"`
	Subject       string             `bson:"subject"`
	Reason        string             `bson:"reason"`
	RevokedBefore time.Time          `bson:"revoked_before"`
	ExpiresAt     time.Time          `bson:"expires_at"` // TTL index
}

// RevokeSubject marks all active sessions for a subject as revoked as of now.
func (s *RevocationStore) RevokeSubject(ctx context.Context, subject, reason string, ttl time.Duration) error {
	if s == nil || s.db == nil || strings.TrimSpace(subject) == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now().UTC()
	_, err := s.subjectsCol().UpdateOne(ctx,
		bson.M{"subject": subject},
		bson.M{
			"$set": bson.M{
				"reason":         reason,
				"revoked_before": now,
				"expires_at":     now.Add(ttl),
			},
			"$setOnInsert": bson.M{"_id": primitive.NewObjectID()},
		},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("revocation_store: revoke subject failed: %w", err)
	}
	log.Printf("[REVOCATION-STORE] Subject revoked: subject=%s reason=%s revoked_before=%s", subject, reason, now.Format(time.RFC3339))
	return nil
}

// IsSubjectRevoked returns true if a subject has a blanket revocation recorded and
// the token was issued before the revocation timestamp.
func (s *RevocationStore) IsSubjectRevoked(ctx context.Context, subject string, issuedAt time.Time) (bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(subject) == "" {
		return false, nil
	}
	var rec RevokedSubjectRecord
	err := s.subjectsCol().FindOne(ctx, bson.M{"subject": subject}).Decode(&rec)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return false, nil
		}
		return false, err
	}
	if issuedAt.IsZero() {
		return true, nil
	}
	return !issuedAt.After(rec.RevokedBefore), nil
}

// Revoke adds a token to the blocklist.
// tokenID is the JWT "jti" claim. expiresAt is when the original token would have expired
// (used for TTL — the blocklist entry is automatically cleaned up after that time).
func (s *RevocationStore) Revoke(ctx context.Context, tokenID, subject, reason string, expiresAt time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	rec := RevokedTokenRecord{
		ID:        primitive.NewObjectID(),
		TokenID:   tokenID,
		Subject:   subject,
		Reason:    reason,
		RevokedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}
	_, err := s.col().InsertOne(ctx, rec)
	if err != nil {
		// If already revoked (duplicate token_id), treat as success
		if strings.Contains(err.Error(), "11000") {
			return nil
		}
		return fmt.Errorf("revocation_store: insert failed: %w", err)
	}
	log.Printf("[REVOCATION-STORE] Token revoked: token_id=%s subject=%s reason=%s", tokenID, subject, reason)
	return nil
}

// IsRevoked returns true if the given token ID appears in the blocklist.
func (s *RevocationStore) IsRevoked(ctx context.Context, tokenID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	count, err := s.col().CountDocuments(ctx, bson.M{"token_id": tokenID})
	if err != nil {
		return false, fmt.Errorf("revocation_store: check failed: %w", err)
	}
	return count > 0, nil
}

// PurgeExpired explicitly removes expired revocation records.
// Normally handled by MongoDB TTL, but this can be called for immediate cleanup.
func (s *RevocationStore) PurgeExpired(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	res, err := s.col().DeleteMany(ctx, bson.M{
		"expires_at": bson.M{"$lt": time.Now().UTC()},
	})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (s *RevocationStore) col() *mongo.Collection {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Collection("revoked_tokens")
}

func (s *RevocationStore) subjectsCol() *mongo.Collection {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Collection("revoked_subjects")
}
