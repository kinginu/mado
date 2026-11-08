package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/usage"
)

// Budgets (DESIGN §17.6, §17.11). A user's month-to-date cost is the sum of their
// usage rows of the UTC calendar month; at or above their budget, chat sends, chat
// session creation, new code sessions on router profiles and messages into them are
// refused. Subscription sessions cost 0 and are never refused.

// monthOf is the UTC calendar month of t, "2026-10". Usage timestamps are RFC 3339
// UTC strings, so a month is the range [month, nextMonth(month)) of strings.
func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

// nextMonth is the month after m ("2026-12" -> "2027-01"); m must be valid.
func nextMonth(m string) string {
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return m
	}
	return t.AddDate(0, 1, 0).Format("2006-01")
}

// validMonth accepts "YYYY-MM".
func validMonth(m string) bool {
	_, err := time.Parse("2006-01", m)
	return err == nil && len(m) == 7
}

// budgetError is a refusal because the budget is used up (HTTP 402).
type budgetError string

func (e budgetError) Error() string { return string(e) }

// UserBudget is the user's own budget, nil when they use the default.
func (s *Store) UserBudget(sub string) (*float64, error) {
	var b sql.NullFloat64
	if err := s.db.QueryRow(`SELECT monthly_budget FROM users WHERE sub=?`, sub).Scan(&b); err != nil {
		return nil, err
	}
	if !b.Valid {
		return nil, nil
	}
	return &b.Float64, nil
}

// SetUserBudget sets (nil: clears) a user's own budget.
func (s *Store) SetUserBudget(sub string, b *float64) error {
	res, err := s.db.Exec(`UPDATE users SET monthly_budget=? WHERE sub=?`, b, sub)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// budgetFor resolves the budget of a user: their own, else default_monthly; limited
// is false when neither is set.
func (h *Hub) budgetFor(own *float64) (limit float64, limited bool) {
	if own != nil {
		return *own, true
	}
	if d := h.cfg.Budgets.DefaultMonthly; d != nil {
		return *d, true
	}
	return 0, false
}

// budgetState is what the settings page, /api/me and the user list show.
type budgetState struct {
	Budget    *float64 `json:"budget"`     // the effective budget; null: no limit
	Own       *float64 `json:"own_budget"` // the user's own; null: the default applies
	MonthCost float64  `json:"month_cost"`
	Over      bool     `json:"over_budget"`
}

func (h *Hub) budgetStateOf(own *float64, monthCost float64) budgetState {
	st := budgetState{Own: own, MonthCost: monthCost}
	if limit, ok := h.budgetFor(own); ok {
		st.Budget = &limit
		st.Over = monthCost >= limit
	}
	return st
}

func (h *Hub) userBudgetState(sub string) budgetState {
	own, _ := h.store.UserBudget(sub)
	return h.budgetStateOf(own, h.store.MonthUsage(sub, monthOf(time.Now())).Cost)
}

// checkBudget refuses work that costs money once the month's budget is used up.
func (h *Hub) checkBudget(sub string) error {
	st := h.userBudgetState(sub)
	if !st.Over {
		return nil
	}
	cur := h.cfg.Budgets.Currency
	return budgetError(fmt.Sprintf("monthly budget reached: %s used of %s this month; it resets on %s-01 (UTC)",
		usage.FormatMoney(cur, st.MonthCost), usage.FormatMoney(cur, *st.Budget), nextMonth(monthOf(time.Now()))))
}

// checkProfileBudget is checkBudget for code work: only router profiles cost money.
func (h *Hub) checkProfileBudget(sub, profile string) error {
	p, err := h.store.Profile(profile)
	if err != nil || p.Kind != proto.KindRouter {
		return nil
	}
	return h.checkBudget(sub)
}

// errStatus is the HTTP status of a refusal: 402 for a budget, def otherwise.
func errStatus(err error, def int) int {
	var be budgetError
	if errors.As(err, &be) {
		return 402
	}
	return def
}
