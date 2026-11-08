package hub

import (
	"encoding/json"

	"github.com/kinginu/mado/internal/usage"
)

// --- hub.toml entries with browser overrides (DESIGN §28.1)
//
// Profiles, chat models and prices share one rule. Each row has its live json, a
// source (config, builtin or ui) and config_json, the current hub.toml version of
// that name ("" when the name is not in hub.toml). The hub never writes hub.toml.

// ovTable names an overridable table and its key column.
type ovTable struct{ table, key string }

var (
	ovProfiles   = ovTable{"profiles", "name"}
	ovChatModels = ovTable{"chat_models", "name"}
	ovPrices     = ovTable{"prices", "model"}
)

// seedEntry is one hub.toml entry: its name and stored JSON.
type seedEntry struct{ Key, JSON string }

// seedOverridable applies the seeding rule at start: every hub.toml entry writes
// config_json, and json and source=config only where the row is absent or its source
// is config, so a ui override survives. An entry that left hub.toml is deleted when
// its source is config and kept, with config_json cleared, when it is ui. Builtin
// rows are not touched.
func (s *Store) seedOverridable(t ovTable, es []seedEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	in := map[string]bool{}
	for _, e := range es {
		in[e.Key] = true
		if _, err := tx.Exec(`INSERT INTO `+t.table+`(`+t.key+`,json,source,config_json) VALUES(?,?,?,?)
			ON CONFLICT(`+t.key+`) DO UPDATE SET config_json=excluded.config_json,
			json=CASE WHEN COALESCE(source,'config')='config' THEN excluded.json ELSE json END,
			source=CASE WHEN COALESCE(source,'config')='config' THEN 'config' ELSE source END`,
			e.Key, e.JSON, SourceConfig, e.JSON); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT ` + t.key + `, COALESCE(source,'config') FROM ` + t.table)
	if err != nil {
		return err
	}
	type kv struct{ key, src string }
	var all []kv
	for rows.Next() {
		var x kv
		if err := rows.Scan(&x.key, &x.src); err != nil {
			rows.Close()
			return err
		}
		all = append(all, x)
	}
	rows.Close()
	for _, x := range all {
		switch {
		case in[x.key]:
		case x.src == SourceConfig:
			_, err = tx.Exec(`DELETE FROM `+t.table+` WHERE `+t.key+`=?`, x.key)
		case x.src == SourceUI:
			_, err = tx.Exec(`UPDATE `+t.table+` SET config_json='' WHERE `+t.key+`=?`, x.key)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// putOverridable stores json under key. Source config also sets config_json (a
// hub.toml version); any other source keeps the row's config_json.
func (s *Store) putOverridable(t ovTable, key, js, source string) error {
	cfg := ""
	if source == SourceConfig {
		cfg = js
	}
	_, err := s.db.Exec(`INSERT INTO `+t.table+`(`+t.key+`,json,source,config_json) VALUES(?,?,?,?)
		ON CONFLICT(`+t.key+`) DO UPDATE SET json=excluded.json, source=excluded.source,
		config_json=CASE WHEN excluded.source='config' THEN excluded.config_json ELSE COALESCE(config_json,'') END`,
		key, js, source, cfg)
	return err
}

// revertOverridable puts a ui override back to its hub.toml version; false when the
// row is not an override of a hub.toml entry.
func (s *Store) revertOverridable(t ovTable, key string) (bool, error) {
	r, err := s.db.Exec(`UPDATE `+t.table+` SET json=config_json, source=? WHERE `+t.key+`=? AND source=? AND COALESCE(config_json,'')<>''`,
		SourceConfig, key, SourceUI)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// deleteUIOnly removes an entry made in the editor that has no hub.toml version.
func (s *Store) deleteUIOnly(t ovTable, key string) (bool, error) {
	r, err := s.db.Exec(`DELETE FROM `+t.table+` WHERE `+t.key+`=? AND source=? AND COALESCE(config_json,'')=''`, key, SourceUI)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// ovState is a row's source and hub.toml version.
type ovState struct{ Source, ConfigJSON string }

// Overrides reports a ui override of a hub.toml entry.
func (o ovState) Overrides() bool { return o.Source == SourceUI && o.ConfigJSON != "" }

// overrideStates maps every key of the table to its state.
func (s *Store) overrideStates(t ovTable) map[string]ovState {
	out := map[string]ovState{}
	rows, err := s.db.Query(`SELECT ` + t.key + `, COALESCE(source,'config'), COALESCE(config_json,'') FROM ` + t.table)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var o ovState
		if rows.Scan(&k, &o.Source, &o.ConfigJSON) == nil {
			out[k] = o
		}
	}
	return out
}

// --- prices (DESIGN §28.2)

// SeedPrices stores the [[prices]] entries under the override rule.
func (s *Store) SeedPrices(ps usage.Prices) error {
	es := make([]seedEntry, 0, len(ps))
	for _, p := range ps {
		b, _ := json.Marshal(p)
		es = append(es, seedEntry{p.Model, string(b)})
	}
	return s.seedOverridable(ovPrices, es)
}

func (s *Store) PutPrice(p usage.Price, source string) error {
	b, _ := json.Marshal(p)
	return s.putOverridable(ovPrices, p.Model, string(b), source)
}

func (s *Store) RevertPrice(model string) (bool, error) { return s.revertOverridable(ovPrices, model) }

func (s *Store) DeletePrice(model string) (bool, error) { return s.deleteUIOnly(ovPrices, model) }

// PriceSource is "" for a model without a row.
func (s *Store) PriceSource(model string) string {
	var src string
	_ = s.db.QueryRow(`SELECT source FROM prices WHERE model=?`, model).Scan(&src)
	return src
}

// Prices lists the price table by model.
func (s *Store) Prices() (usage.Prices, error) {
	rows, err := s.db.Query(`SELECT json FROM prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := usage.Prices{}
	for rows.Next() {
		var j string
		var p usage.Price
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(j), &p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// --- settings (DESIGN §28.3): auto.classifier and concierge.model

// Setting keys that the editor can override.
const (
	SettingClassifier     = "auto.classifier"
	SettingConciergeModel = "concierge.model"
)

// settingKeys are the overridable settings, in editor order.
var settingKeys = []string{SettingClassifier, SettingConciergeModel}

// SettingRow is one settings key: the live value, the hub.toml value and the source.
type SettingRow struct {
	Key, Value, ConfigValue, Source string
}

// SeedSetting stores the hub.toml value of key; a ui value is kept.
func (s *Store) SeedSetting(key, configValue string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value,config_value,source) VALUES(?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET config_value=excluded.config_value,
		value=CASE WHEN source='config' THEN excluded.value ELSE value END`, key, configValue, configValue, SourceConfig)
	return err
}

// Setting returns the row of key; a missing row reads as an empty hub.toml value.
func (s *Store) Setting(key string) SettingRow {
	r := SettingRow{Key: key}
	if s.db.QueryRow(`SELECT value, config_value, source FROM settings WHERE key=?`, key).Scan(&r.Value, &r.ConfigValue, &r.Source) != nil {
		r = SettingRow{Key: key, Source: SourceConfig}
	}
	return r
}

// PutSetting stores a ui value.
func (s *Store) PutSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value,config_value,source) VALUES(?,?,'',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, source=excluded.source`, key, value, SourceUI)
	return err
}

// RevertSetting puts key back to its hub.toml value; false when it was not overridden.
func (s *Store) RevertSetting(key string) (bool, error) {
	r, err := s.db.Exec(`UPDATE settings SET value=config_value, source=? WHERE key=? AND source=?`, SourceConfig, key, SourceUI)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}
