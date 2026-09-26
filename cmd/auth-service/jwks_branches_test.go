package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"wallet-system/pkg/auth"
)

func TestJWKSVerifyTokenBranches(t *testing.T) {
	cache := NewJWKSCache("http://localhost:9999/jwks", 1*time.Hour)
	ctx := context.Background()

	// 1. Token doesn't have 3 parts
	_, err1 := cache.VerifyToken(ctx, "part1.part2")
	if err1 != auth.ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for invalid parts, got %v", err1)
	}

	// 2. Invalid base64 header
	_, err2 := cache.VerifyToken(ctx, "!!!invalid!!!.part2.part3")
	if err2 != auth.ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for bad base64 header, got %v", err2)
	}

	// 3. Invalid JSON header
	badHeaderB64 := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	_, err3 := cache.VerifyToken(ctx, badHeaderB64+".part2.part3")
	if err3 != auth.ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for bad JSON header, got %v", err3)
	}

	// 4. Unsupported algorithm (e.g. HS256)
	hdr := tokenHeader{Alg: "HS256", Typ: "JWT"}
	hdrBytes, _ := json.Marshal(hdr)
	hsHeaderB64 := base64.RawURLEncoding.EncodeToString(hdrBytes)
	_, err4 := cache.VerifyToken(ctx, hsHeaderB64+".part2.part3")
	if err4 == nil || !testing.Verbose() && err4.Error() == "" {
		t.Fatalf("expected unsupported algorithm error, got %v", err4)
	}

	// 5. Exponent 0 in parseRSAPublicKey
	_, errExp := parseRSAPublicKey("AQAB", base64.RawURLEncoding.EncodeToString([]byte{0}))
	if errExp == nil {
		t.Fatal("expected error for zero exponent")
	}
}
