package generator

import (
	"context"
	"net/url"
	"time"

	"chain/core/blocksigner"
	"chain/cos"
	"chain/cos/bc"
	"chain/cos/state"
	"chain/crypto/ed25519"
	"chain/database/pg"
	"chain/errors"
	"chain/log"
)

// TODO(kr): replace RemoteSigners type and use of *blocksigner.Signer
// with a single BlockSigner interface.

// Config encapsulates generator configuration options.
type Config struct {
	RemoteSigners []*RemoteSigner
	LocalSigner   *blocksigner.Signer
	FC            *cos.FC
}

// New constructs a new generator and returns it.
func New(block *bc.Block, snapshot *state.Snapshot, config Config) *Generator {
	return &Generator{
		Config:         config,
		latestBlock:    block,
		latestSnapshot: snapshot,
	}
}

// Generator produces new blocks on an interval.
type Generator struct {
	Config

	// latestBlock and latestSnapshot are current as long as this
	// process remains the leader process. If the process is demoted,
	// generator.Generate() should return and this struct should be
	// garbage collected.
	latestBlock    *bc.Block
	latestSnapshot *state.Snapshot
}

// RemoteSigner defines the address and public key of another Core
// that may sign blocks produced by this generator.
type RemoteSigner struct {
	URL *url.URL
	Key ed25519.PublicKey
}

// Generate runs in a loop, making one new block
// every block period. It returns when its context
// is canceled.
func Generate(ctx context.Context, config Config, period time.Duration) {
	// This process just became leader, so it's responsible
	// for recovering after the previous leader's exit.
	recoveredBlock, recoveredSnapshot, err := config.FC.Recover(ctx)
	if err != nil {
		log.Fatal(ctx, log.KeyError, err)
	}
	g := New(recoveredBlock, recoveredSnapshot, config)

	// Check to see if we already have a pending, generated block.
	// This can happen if the leader process exits between generating
	// the block and committing the signed block to the blockchain.
	b, err := g.getPendingBlock(ctx)
	if err != nil {
		log.Fatal(ctx, err)
	}
	if b != nil && (g.latestBlock == nil || b.Height == g.latestBlock.Height+1) {
		// g.commitBlock will update g.latestBlock and g.latestSnapshot.
		_, err := g.commitBlock(ctx, b)
		if err != nil {
			log.Fatal(ctx, err)
		}
	}

	ticks := time.Tick(period)
	for {
		select {
		case <-ctx.Done():
			log.Messagef(ctx, "Deposed, Generate exiting")
			return
		case <-ticks:
			_, err := g.MakeBlock(ctx)
			if err != nil {
				log.Error(ctx, err)
			}
		}
	}
}

// Submit is an http handler for the generator submit transaction endpoint.
// Other nodes will call this endpoint to notify the generator of submitted
// transactions.
func (g *Config) Submit(ctx context.Context, tx *bc.Tx) error {
	err := g.FC.AddTx(ctx, tx)
	return err
}

// GetBlocks returns blocks (with heights larger than afterHeight) in
// block-height order.
func (g *Config) GetBlocks(ctx context.Context, afterHeight uint64) ([]*bc.Block, error) {
	// TODO(kr): This is not a generator function.
	// Move this to another package.
	err := g.FC.WaitForBlock(ctx, afterHeight+1)
	if err != nil {
		return nil, errors.Wrapf(err, "waiting for block at height %d", afterHeight+1)
	}

	const q = `SELECT data FROM blocks WHERE height > $1 ORDER BY height`
	var blocks []*bc.Block
	err = pg.ForQueryRows(ctx, q, afterHeight, func(b bc.Block) {
		blocks = append(blocks, &b)
	})
	if err != nil {
		return nil, errors.Wrap(err, "querying blocks from the db")
	}
	return blocks, nil
}
