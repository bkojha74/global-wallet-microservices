package merkle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

func generateTestHashes(n int) []string {
	hashes := make([]string, n)
	for i := 0; i < n; i++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("tx_entry_%d", i)))
		hashes[i] = hex.EncodeToString(digest[:])
	}
	return hashes
}

func TestNewTree_EdgeCases(t *testing.T) {
	t.Run("empty leaves returns ErrEmptyTree", func(t *testing.T) {
		_, err := NewTree(nil)
		if err != ErrEmptyTree {
			t.Fatalf("expected ErrEmptyTree, got %v", err)
		}

		_, err = NewTree([]string{})
		if err != ErrEmptyTree {
			t.Fatalf("expected ErrEmptyTree, got %v", err)
		}
	})

	t.Run("single leaf tree", func(t *testing.T) {
		hashes := generateTestHashes(1)
		tree, err := NewTree(hashes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tree.RootHash() != hashes[0] {
			t.Fatalf("expected root %s, got %s", hashes[0], tree.RootHash())
		}
		if len(tree.Leaves) != 1 {
			t.Fatalf("expected 1 leaf, got %d", len(tree.Leaves))
		}
	})

	t.Run("two leaves tree", func(t *testing.T) {
		hashes := generateTestHashes(2)
		tree, err := NewTree(hashes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expectedRoot := HashPair(hashes[0], hashes[1])
		if tree.RootHash() != expectedRoot {
			t.Fatalf("expected root %s, got %s", expectedRoot, tree.RootHash())
		}
	})

	t.Run("odd number of leaves (3 leaves)", func(t *testing.T) {
		hashes := generateTestHashes(3)
		tree, err := NewTree(hashes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tree.RootHash() == "" {
			t.Fatal("expected non-empty root hash")
		}

		// Leaf 3 should be paired with itself at level 1
		p01 := HashPair(hashes[0], hashes[1])
		p22 := HashPair(hashes[2], hashes[2])
		expectedRoot := HashPair(p01, p22)

		if tree.RootHash() != expectedRoot {
			t.Fatalf("expected root %s, got %s", expectedRoot, tree.RootHash())
		}
	})
}

func TestTree_ProofGenerationAndVerification(t *testing.T) {
	leafCounts := []int{1, 2, 3, 4, 7, 8, 15, 16, 31, 32}

	for _, count := range leafCounts {
		t.Run(fmt.Sprintf("%d leaves", count), func(t *testing.T) {
			hashes := generateTestHashes(count)
			tree, err := NewTree(hashes)
			if err != nil {
				t.Fatalf("failed to build tree: %v", err)
			}

			root := tree.RootHash()

			// Test proof for every single leaf
			for i, h := range hashes {
				proof, pErr := tree.GetProof(h)
				if pErr != nil {
					t.Fatalf("failed to get proof for leaf %d (%s): %v", i, h, pErr)
				}

				if proof.LeafHash != h {
					t.Errorf("proof leaf hash mismatch: got %s, want %s", proof.LeafHash, h)
				}
				if proof.RootHash != root {
					t.Errorf("proof root hash mismatch: got %s, want %s", proof.RootHash, root)
				}

				// Valid proof must verify
				if !VerifyProof(h, proof, root) {
					t.Errorf("failed to verify proof for leaf %d (%s)", i, h)
				}

				// Tampered leaf hash must fail verification
				fakeLeaf := hex.EncodeToString(sha256.New().Sum([]byte("fake_leaf")))
				if VerifyProof(fakeLeaf, proof, root) {
					t.Errorf("expected verification to fail for fake leaf on index %d", i)
				}

				// Tampered root must fail verification
				if VerifyProof(h, proof, "corrupted_expected_root") {
					t.Errorf("expected verification to fail with corrupted root on index %d", i)
				}

				// Tampered step sibling must fail verification
				if len(proof.Steps) > 0 {
					tamperedProof := &AuditProof{
						LeafHash: proof.LeafHash,
						RootHash: proof.RootHash,
						Steps:    make([]ProofStep, len(proof.Steps)),
					}
					copy(tamperedProof.Steps, proof.Steps)
					tamperedProof.Steps[0].SiblingHash = "bad_sibling_hash"

					if VerifyProof(h, tamperedProof, root) {
						t.Errorf("expected verification to fail with tampered sibling on index %d", i)
					}
				}
			}

			// Unknown leaf must return ErrLeafNotFound
			unknownHash := hex.EncodeToString(sha256.New().Sum([]byte("unknown_entry")))
			_, err = tree.GetProof(unknownHash)
			if err != ErrLeafNotFound {
				t.Fatalf("expected ErrLeafNotFound for unknown hash, got %v", err)
			}
		})
	}
}

func TestVerifyProof_EdgeCases(t *testing.T) {
	if VerifyProof("", nil, "") {
		t.Error("expected false for nil proof")
	}
	if VerifyProof("abc", nil, "root") {
		t.Error("expected false for nil proof")
	}
	proof := &AuditProof{
		LeafHash: "leaf",
		RootHash: "root",
		Steps:    nil,
	}
	if VerifyProof("leaf", proof, "different_root") {
		t.Error("expected false for mismatched root with no steps")
	}
	if !VerifyProof("leaf", proof, "leaf") {
		t.Error("expected true when leaf is identical to root with no steps")
	}
}
