// Package master — handlers_admin_accounts.go: the cluster admin panel's
// account-management endpoints (list, add credits, suspend, promote,
// reset password). Every handler is gated by requireAdmin.
package master

import (
	"errors"
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// maxManualCreditAdjustment caps a single manual credit change so a fat-
// fingered amount can't grant a tenant a fortune.
const maxManualCreditAdjustment = 100000.0

// adminAccountView is one row of the panel's Accounts tab.
type adminAccountView struct {
	ID             string     `json:"id"`
	Email          string     `json:"email"`
	Credits        float64    `json:"credits"`
	IsAdmin        bool       `json:"is_admin"`
	Suspended      bool       `json:"suspended"`
	LastLogin      *time.Time `json:"last_login,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	TotalSandboxes int        `json:"total_sandboxes"`
}

// adminListAccounts returns every account with its credit balance, admin
// flag, and live (non-destroyed) sandbox count.
func (h *Handlers) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	accounts, err := h.Store.Accounts().List(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminListAccounts", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	sandboxes, err := h.Store.Sandboxes().ListAll(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminListAccounts: sandboxes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	counts := map[string]int{}
	for _, sb := range sandboxes {
		if sb.State == models.SandboxStateDestroyed {
			continue
		}
		counts[sb.AccountID]++
	}
	out := make([]adminAccountView, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, adminAccountView{
			ID:             a.ID,
			Email:          a.Email,
			Credits:        a.CreditsRemaining,
			IsAdmin:        a.IsAdmin,
			Suspended:      a.Suspended,
			LastLogin:      a.LastLogin,
			CreatedAt:      a.CreatedAt,
			TotalSandboxes: counts[a.ID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// addCreditsRequest is the body of POST /v1/admin/accounts/{id}/credits.
type addCreditsRequest struct {
	Amount float64 `json:"amount"`
}

// adminAddCredits manually adjusts an account's prepaid balance. The
// amount may be negative to claw credits back; it reuses the same
// credits_remaining column the billing meter and Stripe webhook drive.
func (h *Handlers) adminAddCredits(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing account id")
		return
	}
	var body addCreditsRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Amount == 0 {
		writeErr(w, http.StatusBadRequest, "amount must be non-zero")
		return
	}
	if body.Amount > maxManualCreditAdjustment || body.Amount < -maxManualCreditAdjustment {
		writeErr(w, http.StatusBadRequest, "amount exceeds the manual adjustment limit")
		return
	}
	if err := h.Store.Accounts().IncrementCredits(r.Context(), id, body.Amount); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		h.log().Error("adminAddCredits", "err", err, "account_id", id)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	acct, err := h.Store.Accounts().GetByID(r.Context(), id)
	if err != nil {
		h.log().Error("adminAddCredits: reload", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log().Info("admin: credits adjusted", "account_id", id, "amount", body.Amount,
		"balance", acct.CreditsRemaining)
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": id,
		"added":      body.Amount,
		"credits":    acct.CreditsRemaining,
	})
}

// adminFlagRequest is the (optional) body of the suspend/promote toggles.
// A nil Value means "flip the current flag"; an explicit value sets it.
type adminFlagRequest struct {
	Suspended *bool `json:"suspended,omitempty"`
	IsAdmin   *bool `json:"is_admin,omitempty"`
}

// adminSuspendAccount toggles (or explicitly sets) an account's suspended
// flag. A suspended account is flagged in the panel for the operator;
// the flag is advisory and does not by itself revoke live sessions.
func (h *Handlers) adminSuspendAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	acct, ok := h.resolveAdminAccount(w, r)
	if !ok {
		return
	}
	var body adminFlagRequest
	if err := decodeBodyOptional(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	target := !acct.Suspended
	if body.Suspended != nil {
		target = *body.Suspended
	}
	if err := h.Store.Accounts().SetSuspended(r.Context(), acct.ID, target); err != nil {
		h.log().Error("adminSuspendAccount", "err", err, "account_id", acct.ID)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log().Info("admin: account suspension changed", "account_id", acct.ID, "suspended", target)
	writeJSON(w, http.StatusOK, map[string]any{"account_id": acct.ID, "suspended": target})
}

// adminPromoteAccount toggles (or explicitly sets) an account's is_admin
// flag, granting or revoking access to this very panel.
func (h *Handlers) adminPromoteAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	acct, ok := h.resolveAdminAccount(w, r)
	if !ok {
		return
	}
	var body adminFlagRequest
	if err := decodeBodyOptional(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	target := !acct.IsAdmin
	if body.IsAdmin != nil {
		target = *body.IsAdmin
	}
	if err := h.Store.Accounts().SetAdmin(r.Context(), acct.ID, target); err != nil {
		h.log().Error("adminPromoteAccount", "err", err, "account_id", acct.ID)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log().Info("admin: account admin flag changed", "account_id", acct.ID, "is_admin", target)
	writeJSON(w, http.StatusOK, map[string]any{"account_id": acct.ID, "is_admin": target})
}

// adminResetPassword sets a fresh random password on an account and
// returns it once, in the clear, for the operator to hand to the user.
// Only the bcrypt hash is persisted.
func (h *Handlers) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	acct, ok := h.resolveAdminAccount(w, r)
	if !ok {
		return
	}
	suffix, err := randomHex(8)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	temp := "Vj-" + suffix // 19 chars, comfortably over minPasswordLen
	hash, err := HashPassword(temp)
	if err != nil {
		h.log().Error("adminResetPassword: hash", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.Store.Accounts().UpdatePassword(r.Context(), acct.ID, hash); err != nil {
		h.log().Error("adminResetPassword", "err", err, "account_id", acct.ID)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log().Info("admin: password reset", "account_id", acct.ID)
	writeJSON(w, http.StatusOK, map[string]string{
		"account_id":         acct.ID,
		"temporary_password": temp,
	})
}

// resolveAdminAccount loads the {id} account and writes a 400/404 on
// failure. It is the shared front of every per-account admin action.
func (h *Handlers) resolveAdminAccount(w http.ResponseWriter, r *http.Request) (*models.Account, bool) {
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing account id")
		return nil, false
	}
	acct, err := h.Store.Accounts().GetByID(r.Context(), id)
	if err != nil {
		writeErr(w, translateStoreErr(err), "account not found")
		return nil, false
	}
	return acct, true
}
