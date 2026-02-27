package wallet

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/gonkalabs/gonka-proxy-go/internal/signer"
)

// Wallet holds a signer and its associated requester address.
type Wallet struct {
	Signer  *signer.Signer
	Address string
}

// Pool manages multiple wallets and routes requests between them
// using atomic round-robin selection.
type Pool struct {
	wallets []Wallet
	counter atomic.Uint64
}

// NewPool creates a Pool from a list of wallets.
// At least one wallet is required.
func NewPool(wallets []Wallet) (*Pool, error) {
	if len(wallets) == 0 {
		return nil, fmt.Errorf("wallet pool: at least one wallet is required")
	}
	slog.Info("wallet pool initialised", "wallets", len(wallets))
	for i, w := range wallets {
		slog.Info("wallet registered", "index", i, "address", w.Address)
	}
	return &Pool{wallets: wallets}, nil
}

// Next returns the next wallet using round-robin selection.
// This is safe for concurrent use.
func (p *Pool) Next() *Wallet {
	idx := p.counter.Add(1) - 1
	return &p.wallets[idx%uint64(len(p.wallets))]
}

// Len returns the number of wallets in the pool.
func (p *Pool) Len() int {
	return len(p.wallets)
}

// All returns all wallets in the pool (e.g. for health checks or diagnostics).
func (p *Pool) All() []Wallet {
	return p.wallets
}
