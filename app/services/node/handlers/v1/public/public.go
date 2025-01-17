// Package public maintains the group of handlers for public access.
package public

import (
	"context"
	"fmt"
	"net/http"
	"time"

	v1 "github.com/ardanlabs/blockchain/business/web/v1"
	"github.com/ardanlabs/blockchain/foundation/blockchain/database"
	"github.com/ardanlabs/blockchain/foundation/blockchain/state"
	"github.com/ardanlabs/blockchain/foundation/events"
	"github.com/ardanlabs/blockchain/foundation/nameservice"
	"github.com/ardanlabs/blockchain/foundation/web"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Handlers manages the set of bar ledger endpoints.
type Handlers struct {
	Log   *zap.SugaredLogger
	State *state.State
	NS    *nameservice.NameService
	WS    websocket.Upgrader
	Evts  *events.Events
}

// Events handles a web socket to provide events to a client.
func (h Handlers) Events(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	v, err := web.GetValues(ctx)
	if err != nil {
		return web.NewShutdownError("web value missing from context")
	}

	h.WS.CheckOrigin = func(r *http.Request) bool { return true }

	c, err := h.WS.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	defer c.Close()

	ch := h.Evts.Acquire(v.TraceID)
	defer h.Evts.Release(v.TraceID)

	ticker := time.NewTicker(time.Second)

	for {
		select {
		case msg, wd := <-ch:
			if !wd {
				return nil
			}

			if err := c.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				return err
			}

		case <-ticker.C:
			if err := c.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				return nil
			}
		}
	}
}

// SubmitWalletTransaction adds new user transactions to the mempool.
func (h Handlers) SubmitWalletTransaction(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	v, err := web.GetValues(ctx)
	if err != nil {
		return web.NewShutdownError("web value missing from context")
	}

	var signedTx database.SignedTx
	if err := web.Decode(r, &signedTx); err != nil {
		return fmt.Errorf("unable to decode payload: %w", err)
	}

	h.Log.Infow("add user tran", "traceid", v.TraceID, "from:nonce", signedTx, "to", signedTx.ToID, "value", signedTx.Value, "tip", signedTx.Tip)
	if err := h.State.UpsertWalletTransaction(signedTx); err != nil {
		return v1.NewRequestError(err, http.StatusBadRequest)
	}

	resp := struct {
		Status string `json:"status"`
	}{
		Status: "transactions added to mempool",
	}

	return web.Respond(ctx, w, resp, http.StatusOK)
}

// Genesis returns the genesis information.
func (h Handlers) Genesis(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	gen := h.State.RetrieveGenesis()
	return web.Respond(ctx, w, gen, http.StatusOK)
}

// Mempool returns the set of uncommitted transactions.
func (h Handlers) Mempool(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	acct := web.Param(r, "account")

	mempool := h.State.RetrieveMempool()

	trans := []tx{}
	for _, tran := range mempool {
		account, _ := tran.FromAccount()
		if acct != "" && ((acct != string(account)) && (acct != string(tran.ToID))) {
			continue
		}

		trans = append(trans, tx{
			FromAccount: account,
			FromName:    h.NS.Lookup(account),
			To:          tran.ToID,
			ToName:      h.NS.Lookup(tran.ToID),
			Nonce:       tran.Nonce,
			Value:       tran.Value,
			Tip:         tran.Tip,
			Data:        tran.Data,
			TimeStamp:   tran.TimeStamp,
			Gas:         tran.Gas,
			Sig:         tran.SignatureString(),
		})
	}

	return web.Respond(ctx, w, trans, http.StatusOK)
}

// Accounts returns the current balances for all users.
func (h Handlers) Accounts(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	accountStr := web.Param(r, "account")

	var accounts map[database.AccountID]database.Account
	switch accountStr {
	case "":
		accounts = h.State.RetrieveAccounts()

	default:
		accountID, err := database.ToAccountID(accountStr)
		if err != nil {
			return err
		}
		account, err := h.State.QueryAccounts(accountID)
		if err != nil {
			return err
		}
		accounts = map[database.AccountID]database.Account{accountID: account}
	}

	resp := make([]act, 0, len(accounts))
	for account, info := range accounts {
		act := act{
			Account: account,
			Name:    h.NS.Lookup(account),
			Balance: info.Balance,
			Nonce:   info.Nonce,
		}
		resp = append(resp, act)
	}

	ai := actInfo{
		LastestBlock: h.State.RetrieveLatestBlock().Hash(),
		Uncommitted:  len(h.State.RetrieveMempool()),
		Accounts:     resp,
	}

	return web.Respond(ctx, w, ai, http.StatusOK)
}

// BlocksByAccount returns all the blocks and their details.
func (h Handlers) BlocksByAccount(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	accountStr, err := database.ToAccountID(web.Param(r, "account"))
	if err != nil {
		return err
	}

	dbBlocks := h.State.QueryBlocksByAccount(accountStr)
	if len(dbBlocks) == 0 {
		return web.Respond(ctx, w, nil, http.StatusNoContent)
	}

	blocks := make([]block, len(dbBlocks))
	for j, blk := range dbBlocks {
		values := blk.Trans.Values()
		trans := make([]tx, len(blk.Trans.Values()))

		for i, tran := range values {
			account, err := tran.FromAccount()
			if err != nil {
				return err
			}

			trans[i] = tx{
				FromAccount: account,
				FromName:    h.NS.Lookup(account),
				To:          tran.ToID,
				ToName:      h.NS.Lookup(tran.ToID),
				Nonce:       tran.Nonce,
				Value:       tran.Value,
				Tip:         tran.Tip,
				Data:        tran.Data,
				TimeStamp:   tran.TimeStamp,
				Gas:         tran.Gas,
				Sig:         tran.SignatureString(),
			}
		}

		b := block{
			ParentHash:   blk.Header.ParentHash,
			MinerAccount: blk.Header.MinerAccountID,
			Difficulty:   blk.Header.Difficulty,
			Number:       blk.Header.Number,
			TimeStamp:    blk.Header.TimeStamp,
			Nonce:        blk.Header.Nonce,
			Transactions: trans,
		}

		blocks[j] = b
	}

	return web.Respond(ctx, w, blocks, http.StatusOK)
}
