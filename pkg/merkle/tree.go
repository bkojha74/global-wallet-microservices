package merkle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

var (
	// ErrEmptyTree indicates that an operation was attempted on an empty Merkle tree.
	ErrEmptyTree = errors.New("merkle: cannot construct tree with zero leaves")
	// ErrLeafNotFound indicates that the requested leaf hash was not found in the Merkle tree.
	ErrLeafNotFound = errors.New("merkle: leaf hash not found in tree")
	// ErrInvalidProof indicates that the provided proof is malformed or invalid.
	ErrInvalidProof = errors.New("merkle: invalid audit proof")
)

// Node represents an internal node or leaf in the binary Merkle tree.
type Node struct {
	Hash  string
	Left  *Node
	Right *Node
}

// ProofStep represents a single hop in the cryptographic audit path from a leaf to the Merkle root.
type ProofStep struct {
	SiblingHash string `json:"sibling_hash"`
	IsRight     bool   `json:"is_right"` // true if the sibling is on the right of the current hash
}

// AuditProof contains the complete cryptographic path required to prove that a specific leaf exists in the tree.
type AuditProof struct {
	LeafHash string      `json:"leaf_hash"`
	RootHash string      `json:"root_hash"`
	Steps    []ProofStep `json:"steps"`
}

// Tree represents a balanced binary Merkle tree computed over immutable hashes.
type Tree struct {
	Root   *Node
	Leaves []*Node
}

// HashPair computes the SHA-256 digest over the concatenation of two hexadecimal child hashes,
// using the RFC 6962 internal node domain separator (0x01) to prevent second-preimage attacks.
func HashPair(leftHex, rightHex string) string {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write([]byte(leftHex))
	h.Write([]byte(rightHex))
	digest := h.Sum(nil)
	return hex.EncodeToString(digest)
}

// HashLeaf computes the SHA-256 digest of raw data to form a leaf hash,
// using the RFC 6962 leaf domain separator (0x00).
func HashLeaf(data []byte) string {
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(data)
	digest := h.Sum(nil)
	return hex.EncodeToString(digest)
}

// NewTree constructs a binary Merkle tree from a slice of leaf hashes.
// If an odd number of nodes exists at any level, the odd node is promoted directly
// to the next level without duplication, preventing Bitcoin CVE-2012-2459 collision flaws.
func NewTree(leafHashes []string) (*Tree, error) {
	if len(leafHashes) == 0 {
		return nil, ErrEmptyTree
	}

	leaves := make([]*Node, len(leafHashes))
	for i, h := range leafHashes {
		cleanHash := strings.ToLower(strings.TrimSpace(h))
		leaves[i] = &Node{Hash: cleanHash}
	}

	if len(leaves) == 1 {
		return &Tree{
			Root:   leaves[0],
			Leaves: leaves,
		}, nil
	}

	currentLevel := leaves
	for len(currentLevel) > 1 {
		var nextLevel []*Node
		numNodes := len(currentLevel)

		for i := 0; i < numNodes; i += 2 {
			if i+1 < numNodes {
				left := currentLevel[i]
				right := currentLevel[i+1]
				parentHash := HashPair(left.Hash, right.Hash)
				parentNode := &Node{
					Hash:  parentHash,
					Left:  left,
					Right: right,
				}
				nextLevel = append(nextLevel, parentNode)
			} else {
				// Promote odd node directly to the next level without duplication
				nextLevel = append(nextLevel, currentLevel[i])
			}
		}

		currentLevel = nextLevel
	}

	return &Tree{
		Root:   currentLevel[0],
		Leaves: leaves,
	}, nil
}

// RootHash returns the hexadecimal SHA-256 Merkle root hash.
func (t *Tree) RootHash() string {
	if t == nil || t.Root == nil {
		return ""
	}
	return t.Root.Hash
}

// GetProof constructs a cryptographic audit proof for the specified leaf hash.
func (t *Tree) GetProof(leafHash string) (*AuditProof, error) {
	if t == nil || t.Root == nil {
		return nil, ErrEmptyTree
	}

	cleanTarget := strings.ToLower(strings.TrimSpace(leafHash))
	var steps []ProofStep

	found := findPath(t.Root, cleanTarget, &steps)
	if !found {
		return nil, ErrLeafNotFound
	}

	return &AuditProof{
		LeafHash: cleanTarget,
		RootHash: t.Root.Hash,
		Steps:    steps,
	}, nil
}

// findPath performs a post-order traversal to collect sibling nodes from leaf to root.
func findPath(current *Node, target string, steps *[]ProofStep) bool {
	if current == nil {
		return false
	}

	// Leaf node check
	if current.Left == nil && current.Right == nil {
		return current.Hash == target
	}

	// Check if target is in left subtree
	if findPath(current.Left, target, steps) {
		*steps = append(*steps, ProofStep{
			SiblingHash: current.Right.Hash,
			IsRight:     true, // sibling is on the right
		})
		return true
	}

	// Check if target is in right subtree
	if findPath(current.Right, target, steps) {
		*steps = append(*steps, ProofStep{
			SiblingHash: current.Left.Hash,
			IsRight:     false, // sibling is on the left
		})
		return true
	}

	return false
}

// VerifyProof verifies that the given leaf hash produces the expected Merkle root using the audit proof steps.
func VerifyProof(leafHash string, proof *AuditProof, expectedRoot string) bool {
	if proof == nil || leafHash == "" || expectedRoot == "" {
		return false
	}

	current := strings.ToLower(strings.TrimSpace(leafHash))
	cleanExpected := strings.ToLower(strings.TrimSpace(expectedRoot))

	for _, step := range proof.Steps {
		sibling := strings.ToLower(strings.TrimSpace(step.SiblingHash))
		if step.IsRight {
			current = HashPair(current, sibling)
		} else {
			current = HashPair(sibling, current)
		}
	}

	return current == cleanExpected
}
