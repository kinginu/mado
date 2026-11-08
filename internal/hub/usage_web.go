package hub

import (
	"database/sql"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The usage page and export (DESIGN §17.6, §17.11): a user sees their own rows; an
// admin any user's, or everyone's with totals per user.

// UsageGroup is one line of a usage table: totals per model and kind, or per user.
type UsageGroup struct {
	Model string `json:"model,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Sub   string `json:"sub,omitempty"`
	UsageTotals
}

// usageScope is the WHERE clause of one month, narrowed to a user unless sub is "".
func usageScope(month, sub string) (string, []any) {
	where, args := `ts>=? AND ts<?`, []any{month, nextMonth(month)}
	if sub != "" {
		where, args = where+` AND owner_sub=?`, append(args, sub)
	}
	return where, args
}

const usageSums = `COALESCE(SUM(input),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
	COALESCE(SUM(output),0), COALESCE(SUM(cost_usd),0), COUNT(*)`

// UsageGroups sums a month's rows by "model" (model and kind) or by "user".
func (s *Store) UsageGroups(month, sub, by string) ([]UsageGroup, error) {
	where, args := usageScope(month, sub)
	cols, group := `COALESCE(model,''), COALESCE(kind,''), ''`, `model, kind`
	if by == "user" {
		cols, group = `'', '', COALESCE(owner_sub,'')`, `owner_sub`
	}
	rows, err := s.db.Query(`SELECT `+cols+`, `+usageSums+` FROM usage WHERE `+where+` GROUP BY `+group+
		` ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageGroup{}
	for rows.Next() {
		var g UsageGroup
		if err := rows.Scan(&g.Model, &g.Kind, &g.Sub, &g.Input, &g.CacheRead, &g.CacheWr, &g.Output, &g.Cost, &g.Responses); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UsageTotal sums a month's rows.
func (s *Store) UsageTotal(month, sub string) UsageTotals {
	where, args := usageScope(month, sub)
	return s.usageWhere(where, args...)
}

// UsageMonths lists the months with usage rows, newest first.
func (s *Store) UsageMonths(sub string) []string {
	q, args := `SELECT DISTINCT substr(ts,1,7) FROM usage`, []any{}
	if sub != "" {
		q, args = q+` WHERE owner_sub=?`, append(args, sub)
	}
	rows, err := s.db.Query(q+` ORDER BY 1 DESC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if rows.Scan(&m) == nil && validMonth(m) {
			out = append(out, m)
		}
	}
	return out
}

// EachUsageRow calls fn for every row of a month in time order (the CSV export).
func (s *Store) EachUsageRow(month, sub string, fn func(UsageRow) error) error {
	where, args := usageScope(month, sub)
	rows, err := s.db.Query(`SELECT ts, COALESCE(session_id,''), COALESCE(kind,''), COALESCE(owner_sub,''), COALESCE(model,''),
		COALESCE(message_id,''), input, cache_read, cache_write, output, cost_usd FROM usage WHERE `+where+` ORDER BY ts, id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.TS, &u.SessionID, &u.Kind, &u.OwnerSub, &u.Model, &u.MessageID, &u.Input, &u.CacheRead,
			&u.CacheWrite, &u.Output, &u.Cost); err != nil {
			return err
		}
		if err := fn(u); err != nil {
			return err
		}
	}
	return rows.Err()
}

// UserRow is a user as the admin list shows them.
type UserRow struct {
	Sub, Email, Name, CreatedAt, LastLogin string
	IsAdmin                                bool
	Budget                                 *float64 // own budget, nil: the default
}

// Users lists every user by name.
func (s *Store) Users() ([]UserRow, error) {
	rows, err := s.db.Query(`SELECT sub, COALESCE(email,''), COALESCE(name,''), COALESCE(created_at,''), COALESCE(last_login,''),
		COALESCE(is_admin,0), monthly_budget FROM users ORDER BY lower(COALESCE(NULLIF(name,''), email, sub))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var u UserRow
		var b sql.NullFloat64
		if err := rows.Scan(&u.Sub, &u.Email, &u.Name, &u.CreatedAt, &u.LastLogin, &u.IsAdmin, &b); err != nil {
			return nil, err
		}
		if b.Valid {
			u.Budget = &b.Float64
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// usageQuery reads ?month= and ?user= with the access rule: users see only
// themselves; an admin sees anyone, and everyone when user is empty.
func (h *Hub) usageQuery(w http.ResponseWriter, r *http.Request, ws *WebSession) (month, sub string, ok bool) {
	month = r.URL.Query().Get("month")
	if month == "" {
		month = monthOf(time.Now())
	}
	if !validMonth(month) {
		apiError(w, http.StatusBadRequest, "month must be YYYY-MM")
		return "", "", false
	}
	sub = r.URL.Query().Get("user")
	if !ws.User.IsAdmin {
		if sub != "" && sub != ws.User.Sub {
			apiError(w, http.StatusForbidden, "you can only see your own usage")
			return "", "", false
		}
		sub = ws.User.Sub
	}
	return month, sub, true
}

type usagePerson struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (h *Hub) apiUsage(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	month, sub, ok := h.usageQuery(w, r, ws)
	if !ok {
		return
	}
	models, err := h.store.UsageGroups(month, sub, "model")
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	months := h.store.UsageMonths(sub)
	if cur := monthOf(time.Now()); len(months) == 0 || months[0] < cur {
		months = append([]string{cur}, months...)
	}
	out := map[string]any{"month": month, "user": sub, "currency": h.cfg.Budgets.Currency, "months": months,
		"models": models, "total": h.store.UsageTotal(month, sub)}
	if sub != "" && month == monthOf(time.Now()) {
		out["budget"] = h.userBudgetState(sub)
	}
	if ws.User.IsAdmin {
		users, _ := h.store.Users()
		people := make([]usagePerson, 0, len(users))
		for _, u := range users {
			people = append(people, usagePerson{u.Sub, u.Name, u.Email})
		}
		out["people"] = people
		if sub == "" {
			byUser, _ := h.store.UsageGroups(month, "", "user")
			out["users"] = byUser
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// csvCell keeps a spreadsheet from reading a cell as a formula.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *Hub) apiUsageCSV(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	month, sub, ok := h.usageQuery(w, r, ws)
	if !ok {
		return
	}
	emails := map[string]string{}
	if users, err := h.store.Users(); err == nil {
		for _, u := range users {
			emails[u.Sub] = u.Email
		}
	}
	name := "mado-usage-" + month
	if sub != "" && sub != ws.User.Sub {
		name += "-" + strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, sub)
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	cur := h.cfg.Budgets.Currency
	_ = cw.Write([]string{"ts", "user", "email", "kind", "session_id", "model", "message_id", "input", "cache_read",
		"cache_write", "output", "cost", "currency"})
	i := strconv.FormatInt
	err := h.store.EachUsageRow(month, sub, func(u UsageRow) error {
		return cw.Write([]string{u.TS, csvCell(u.OwnerSub), csvCell(emails[u.OwnerSub]), u.Kind, u.SessionID, csvCell(u.Model),
			csvCell(u.MessageID), i(u.Input, 10), i(u.CacheRead, 10), i(u.CacheWrite, 10), i(u.Output, 10),
			strconv.FormatFloat(u.Cost, 'f', -1, 64), cur})
	})
	if err != nil {
		h.log.Error("usage csv", "err", err)
	}
	cw.Flush()
}
