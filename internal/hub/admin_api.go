package hub

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Admin JSON API (DESIGN §17.11): users and budgets here; profiles, chat models,
// history and enrollment in their own files.

// requireAdminAPI is requireAPI for admins only.
func (h *Hub) requireAdminAPI(next func(http.ResponseWriter, *http.Request, *WebSession)) http.HandlerFunc {
	return h.requireAPI(func(w http.ResponseWriter, r *http.Request, ws *WebSession) {
		if !ws.User.IsAdmin {
			apiError(w, http.StatusForbidden, "admins only")
			return
		}
		next(w, r, ws)
	})
}

type apiUser struct {
	Sub       string `json:"sub"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	IsAdmin   bool   `json:"is_admin"`
	CreatedAt string `json:"created_at"`
	LastLogin string `json:"last_login"`
	budgetState
}

func (h *Hub) apiUserView(u UserRow, monthCost float64) apiUser {
	return apiUser{Sub: u.Sub, Name: u.Name, Email: u.Email, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt, LastLogin: u.LastLogin,
		budgetState: h.budgetStateOf(u.Budget, monthCost)}
}

func (h *Hub) apiAdminUsers(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	users, err := h.store.Users()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	month := monthOf(time.Now())
	costs := map[string]float64{}
	if gs, err := h.store.UsageGroups(month, "", "user"); err == nil {
		for _, g := range gs {
			costs[g.Sub] = g.Cost
		}
	}
	out := make([]apiUser, 0, len(users))
	for _, u := range users {
		out = append(out, h.apiUserView(u, costs[u.Sub]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "month": month, "currency": h.cfg.Budgets.Currency,
		"default_budget": h.cfg.Budgets.DefaultMonthly})
}

// apiAdminPatchUser sets a user's own budget: {"monthly_budget": 1500} or null for the
// default. The admin flag comes from the IdP group at each login and is not editable.
func (h *Hub) apiAdminPatchUser(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	sub := r.PathValue("sub")
	var q map[string]json.RawMessage
	if !decodeBody(w, r, &q) {
		return
	}
	raw, ok := q["monthly_budget"]
	if !ok || len(q) != 1 {
		apiError(w, http.StatusBadRequest, `only "monthly_budget" can be changed`)
		return
	}
	var b *float64
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var v float64
		if json.Unmarshal(raw, &v) != nil || v < 0 || math.IsInf(v, 0) || v > 1e12 {
			apiError(w, http.StatusBadRequest, "monthly_budget must be a number >= 0, or null for the default")
			return
		}
		b = &v
	}
	if err := h.store.SetUserBudget(sub, b); err != nil {
		apiError(w, http.StatusNotFound, "no such user")
		return
	}
	detail := sub + " default"
	if b != nil {
		detail = sub + " " + strconv.FormatFloat(*b, 'f', -1, 64)
	}
	h.store.Audit(ws.User.Sub, "budget_set", detail)
	users, _ := h.store.Users()
	for _, u := range users {
		if u.Sub == sub {
			writeJSON(w, http.StatusOK, h.apiUserView(u, h.store.MonthUsage(sub, monthOf(time.Now())).Cost))
			return
		}
	}
	apiError(w, http.StatusNotFound, "no such user")
}
