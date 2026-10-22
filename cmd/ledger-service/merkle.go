package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"wallet-system/pkg/merkle"
)

// MerkleCheckpoint represents an immutable cryptographic batch audit root
// computed across a sequential range of ledger journal entries.
type MerkleCheckpoint struct {
	CheckpointID  string    `bson:"checkpoint_id" json:"checkpoint_id"`
	StartSequence int64     `bson:"start_sequence" json:"start_sequence"`
	EndSequence   int64     `bson:"end_sequence" json:"end_sequence"`
	MerkleRoot    string    `bson:"merkle_root" json:"merkle_root"`
	EntryCount    int       `bson:"entry_count" json:"entry_count"`
	CreatedAt     time.Time `bson:"created_at" json:"created_at"`
	LeafHashes    []string  `bson:"leaf_hashes" json:"leaf_hashes"`
}

// MerkleProofResponse represents the response containing cryptographic inclusion proof for an entry.
type MerkleProofResponse struct {
	EntryHash    string             `json:"entry_hash"`
	CheckpointID string             `json:"checkpoint_id"`
	MerkleRoot   string             `json:"merkle_root"`
	Proof        *merkle.AuditProof `json:"proof"`
	Verified     bool               `json:"verified"`
}

// GenerateCheckpoint creates a new MerkleCheckpoint over uncheckpointed ledger entries.
func GenerateCheckpoint(ctx context.Context, db *mongo.Database, batchLimit int) (*MerkleCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("database client is nil")
	}

	if batchLimit <= 0 {
		batchLimit = 1000
	}

	cpCol := db.Collection("ledger_merkle_roots")
	entriesCol := db.Collection("ledger_entries")

	// Determine last checkpointed sequence number
	var lastSeq int64 = 0
	findOpt := options.FindOne().SetSort(bson.D{bson.E{Key: "end_sequence", Value: -1}})
	var lastCP MerkleCheckpoint
	if err := cpCol.FindOne(ctx, bson.M{}, findOpt).Decode(&lastCP); err == nil {
		lastSeq = lastCP.EndSequence
	}

	// Query next batch of sequential ledger entries
	cursor, err := entriesCol.Find(ctx,
		bson.M{"sequence_number": bson.M{"$gt": lastSeq}},
		options.Find().
			SetSort(bson.D{bson.E{Key: "sequence_number", Value: 1}}).
			SetLimit(int64(batchLimit)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query uncheckpointed ledger entries: %w", err)
	}
	defer cursor.Close(ctx)

	var leafHashes []string
	var startSeq, endSeq int64

	for cursor.Next(ctx) {
		var doc LedgerDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("failed to decode ledger entry: %w", err)
		}

		hash := doc.EntryHash
		if hash == "" {
			hash = doc.IdempotencyKey
		}
		leafHashes = append(leafHashes, hash)

		if startSeq == 0 {
			startSeq = doc.SequenceNumber
		}
		endSeq = doc.SequenceNumber
	}

	if len(leafHashes) == 0 {
		return nil, nil // No new entries to checkpoint
	}

	tree, err := merkle.NewTree(leafHashes)
	if err != nil {
		return nil, fmt.Errorf("failed to construct merkle tree: %w", err)
	}

	cp := &MerkleCheckpoint{
		CheckpointID:  fmt.Sprintf("mcp_%d_%d_%d", time.Now().Unix(), startSeq, endSeq),
		StartSequence: startSeq,
		EndSequence:   endSeq,
		MerkleRoot:    tree.RootHash(),
		EntryCount:    len(leafHashes),
		CreatedAt:     time.Now().UTC(),
		LeafHashes:    leafHashes,
	}

	if _, err := cpCol.InsertOne(ctx, cp); err != nil {
		return nil, fmt.Errorf("failed to persist merkle checkpoint: %w", err)
	}

	log.Printf("[LEDGER-MERKLE] Created checkpoint %s (seq %d -> %d, root: %s, entries: %d)",
		cp.CheckpointID, cp.StartSequence, cp.EndSequence, cp.MerkleRoot, cp.EntryCount)

	return cp, nil
}

// GetLatestCheckpoint retrieves the most recently committed Merkle checkpoint.
func GetLatestCheckpoint(ctx context.Context, db *mongo.Database) (*MerkleCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("database client is nil")
	}
	cpCol := db.Collection("ledger_merkle_roots")
	findOpt := options.FindOne().SetSort(bson.D{bson.E{Key: "end_sequence", Value: -1}})
	var cp MerkleCheckpoint
	if err := cpCol.FindOne(ctx, bson.M{}, findOpt).Decode(&cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// GetCheckpoints returns a list of recent checkpoints ordered by end_sequence descending.
func GetCheckpoints(ctx context.Context, db *mongo.Database, limit int) ([]MerkleCheckpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("database client is nil")
	}
	if limit <= 0 {
		limit = 50
	}
	cpCol := db.Collection("ledger_merkle_roots")
	findOpts := options.Find().
		SetSort(bson.D{bson.E{Key: "end_sequence", Value: -1}}).
		SetLimit(int64(limit))

	cursor, err := cpCol.Find(ctx, bson.M{}, findOpts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var list []MerkleCheckpoint
	for cursor.Next(ctx) {
		var cp MerkleCheckpoint
		if err := cursor.Decode(&cp); err == nil {
			list = append(list, cp)
		}
	}
	return list, nil
}

// GenerateProofForEntry finds the checkpoint containing entryHash and produces an inclusion proof.
func GenerateProofForEntry(ctx context.Context, db *mongo.Database, entryHash string, checkpointID string) (*MerkleProofResponse, error) {
	if db == nil {
		return nil, fmt.Errorf("database client is nil")
	}

	cleanHash := strings.ToLower(strings.TrimSpace(entryHash))
	if cleanHash == "" {
		return nil, fmt.Errorf("entry_hash is required")
	}

	cpCol := db.Collection("ledger_merkle_roots")
	var cp MerkleCheckpoint

	if checkpointID != "" {
		if err := cpCol.FindOne(ctx, bson.M{"checkpoint_id": checkpointID}).Decode(&cp); err != nil {
			return nil, fmt.Errorf("checkpoint %q not found: %w", checkpointID, err)
		}
	} else {
		// Search for checkpoint containing this leaf hash
		filter := bson.M{"leaf_hashes": cleanHash}
		findOpts := options.FindOne().SetSort(bson.D{bson.E{Key: "end_sequence", Value: -1}})
		if err := cpCol.FindOne(ctx, filter, findOpts).Decode(&cp); err != nil {
			// Fallback: check latest checkpoint
			latest, lErr := GetLatestCheckpoint(ctx, db)
			if lErr != nil {
				return nil, fmt.Errorf("entry hash %s not found in any checkpoint", cleanHash)
			}
			cp = *latest
		}
	}

	tree, err := merkle.NewTree(cp.LeafHashes)
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct merkle tree: %w", err)
	}

	proof, err := tree.GetProof(cleanHash)
	if err != nil {
		return nil, fmt.Errorf("failed to compute audit proof for entry %s: %w", cleanHash, err)
	}

	verified := merkle.VerifyProof(cleanHash, proof, cp.MerkleRoot)

	return &MerkleProofResponse{
		EntryHash:    cleanHash,
		CheckpointID: cp.CheckpointID,
		MerkleRoot:   cp.MerkleRoot,
		Proof:        proof,
		Verified:     verified,
	}, nil
}

// StartMerkleWorker initiates the background worker periodically creating Merkle checkpoints.
func StartMerkleWorker(ctx context.Context, db *mongo.Database, interval time.Duration, batchLimit int) {
	if db == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if batchLimit <= 0 {
		batchLimit = 1000
	}

	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		log.Printf("[LEDGER-MERKLE] Checkpoint worker started (interval=%v, batchLimit=%d)", interval, batchLimit)
		for {
			select {
			case <-ctx.Done():
				log.Println("[LEDGER-MERKLE] Checkpoint worker stopped")
				return
			case <-ticker.C:
				workCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, err := GenerateCheckpoint(workCtx, db, batchLimit)
				if err != nil {
					log.Printf("[LEDGER-MERKLE] Checkpoint error: %v", err)
				}
				cancel()
			}
		}
	}()
}
