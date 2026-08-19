package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
	ModeVerify Mode = "verify"
)

type Options struct{ Mode Mode }
type Issue struct {
	Level  string `json:"level"`
	Code   string `json:"code"`
	Source string `json:"source,omitempty"`
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail"`
}
type Counts struct {
	Categories int `json:"categories"`
	Plugins    int `json:"plugins"`
	Versions   int `json:"versions"`
	Audits     int `json:"audits"`
	Inserted   int `json:"inserted,omitempty"`
	Existing   int `json:"existing,omitempty"`
	Missing    int `json:"missing,omitempty"`
	Conflicts  int `json:"conflicts,omitempty"`
}
type Report struct {
	Mode         Mode      `json:"mode"`
	Expected     Counts    `json:"expected"`
	Observed     Counts    `json:"observed"`
	ExpectedHash string    `json:"expected_hash"`
	ObservedHash string    `json:"observed_hash,omitempty"`
	Issues       []Issue   `json:"issues"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}
type catRow struct {
	id, name, icon, types string
	order, status         int
	created, updated      time.Time
	deleted               sql.NullTime
}
type plugRow struct {
	id, name, typ, cat, tags, publisher, owner, space, visibility, creator, by, botUID, botName, manifest, pkg, mhash, phash, versionID string
	status                                                                                                                              int
	created, updated                                                                                                                    time.Time
	deleted                                                                                                                             sql.NullTime
}
type verRow struct {
	id, pid, version, manifest, pkg, mhash, phash, changelog, by string
	created                                                      time.Time
}
type plan struct {
	cats     []catRow
	plugins  []plugRow
	versions []verRow
	issues   []Issue
}
type Runner struct {
	db  *sql.DB
	now func() time.Time
}

func New(db *sql.DB) *Runner {
	return &Runner{db: db, now: func() time.Time { return time.Now().UTC() }}
}
func (r *Runner) Run(ctx context.Context, o Options) (Report, error) {
	if o.Mode == "" {
		o.Mode = ModeDryRun
	}
	if o.Mode != ModeDryRun && o.Mode != ModeApply && o.Mode != ModeVerify {
		return Report{}, fmt.Errorf("invalid mode %q", o.Mode)
	}
	start := r.now()
	p, e := r.build(ctx)
	if e != nil {
		return Report{}, e
	}
	rep := Report{Mode: o.Mode, StartedAt: start, Issues: p.issues, Expected: Counts{Categories: len(p.cats), Plugins: len(p.plugins), Versions: len(p.versions), Audits: len(p.plugins)}, ExpectedHash: p.hash()}
	if o.Mode == ModeApply {
		if e = r.apply(ctx, p, &rep); e != nil {
			return Report{}, e
		}
	}
	if o.Mode != ModeDryRun {
		rep.Observed, rep.ObservedHash, rep.Issues, e = r.verify(ctx, p, rep.Issues)
		if e != nil {
			return Report{}, e
		}
	}
	rep.FinishedAt = r.now()
	return rep, nil
}
func (r *Runner) build(ctx context.Context) (plan, error) {
	var p plan
	counts, e := r.idCounts(ctx)
	if e != nil {
		return p, e
	}
	cats, e := r.categories(ctx, &p)
	if e != nil {
		return p, e
	}
	tags, e := r.tags(ctx)
	if e != nil {
		return p, e
	}
	if e = r.skills(ctx, counts, cats, tags, &p); e != nil {
		return p, e
	}
	if e = r.mcps(ctx, counts, &p); e != nil {
		return p, e
	}
	p.issues = append(p.issues, Issue{"skip", "expert_graph_not_migrated", "experts", "", "expert graph requires separately reviewed mapping"}, Issue{"skip", "expert_graph_not_migrated", "expert_squads", "", "members are snapshots, not proven expert references"}, Issue{"skip", "placements_not_migrated", "legacy catalogs", "", "no confirmed placement_code mapping"})
	sort.Slice(p.cats, func(i, j int) bool { return p.cats[i].id < p.cats[j].id })
	sort.Slice(p.plugins, func(i, j int) bool { return p.plugins[i].id < p.plugins[j].id })
	sort.Slice(p.versions, func(i, j int) bool { return p.versions[i].id < p.versions[j].id })
	return p, nil
}
func (r *Runner) idCounts(ctx context.Context) (map[string]int, error) {
	rs, e := r.db.QueryContext(ctx, `SELECT id,COUNT(*) FROM (SELECT id FROM skills UNION ALL SELECT id FROM experts UNION ALL SELECT id FROM expert_squads UNION ALL SELECT id FROM mcp_servers) s GROUP BY id`)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	m := map[string]int{}
	for rs.Next() {
		var id string
		var n int
		if e = rs.Scan(&id, &n); e != nil {
			return nil, e
		}
		m[id] = n
	}
	return m, rs.Err()
}
func active(d sql.NullTime) int {
	if d.Valid {
		return 0
	}
	return 1
}
func nstr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func ntime(t sql.NullTime) any {
	if t.Valid {
		return t.Time
	}
	return nil
}
func both(a, b []byte) string {
	return hashJSON(append(append(append([]byte{}, a...), byte('\n')), b...))
}
func (r *Runner) categories(ctx context.Context, p *plan) (map[string]string, error) {
	m := map[string]string{}
	for _, s := range []struct {
		table, family string
		types         []string
	}{{"categories", "skillcat", []string{"skill"}}, {"expert_categories", "expertcat", []string{"expert", "expert_team"}}} {
		rs, e := r.db.QueryContext(ctx, `SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM `+s.table)
		if e != nil {
			return nil, e
		}
		for rs.Next() {
			var old, n, i string
			var o int
			var c, u time.Time
			var d sql.NullTime
			if e = rs.Scan(&old, &n, &i, &o, &c, &u, &d); e != nil {
				rs.Close()
				return nil, e
			}
			id := DeterministicID(s.family, old)
			t, _ := json.Marshal(s.types)
			m[s.table+":"+old] = id
			p.cats = append(p.cats, catRow{id, n, i, string(t), o, active(d), c, u, d})
		}
		rs.Close()
	}
	return m, nil
}
func (r *Runner) tags(ctx context.Context) (map[int64]string, error) {
	rs, e := r.db.QueryContext(ctx, `SELECT id,name FROM skill_tags`)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	m := map[int64]string{}
	for rs.Next() {
		var id int64
		var n string
		if e = rs.Scan(&id, &n); e != nil {
			return nil, e
		}
		m[id] = n
	}
	return m, rs.Err()
}
func mkver(id, pid, v, ch, by string, c time.Time, m, p []byte) verRow {
	return verRow{id, pid, v, string(m), string(p), hashJSON(m), both(m, p), ch, by, c}
}

func (r *Runner) skills(ctx context.Context, counts map[string]int, cats map[string]string, dict map[int64]string, p *plan) error {
	q := `SELECT id,name,display_name,icon_url,source_skill_id,current_version_id,description,category_id,tags,owner_id,owner_name,creator_id,creator_name,space_id,visibility,version,readme_content,file_name,file_url,file_size,file_sha256,created_at,updated_at,is_deleted FROM skills`
	rs, e := r.db.QueryContext(ctx, q)
	if e != nil {
		return e
	}
	defer rs.Close()
	for rs.Next() {
		var id, n, display, icon, source, current, desc, cat, raw, owner, ownerName, creatorID, creator, space, visibility, v, readme, fileName, fileURL, sha string
		var size int64
		var c, u time.Time
		var deleted bool
		if e = rs.Scan(&id, &n, &display, &icon, &source, &current, &desc, &cat, &raw, &owner, &ownerName, &creatorID, &creator, &space, &visibility, &v, &readme, &fileName, &fileURL, &size, &sha, &c, &u, &deleted); e != nil {
			return e
		}
		tags, e := namesFromTagIDs([]byte(raw), dict)
		if e != nil {
			p.issues = append(p.issues, Issue{"skip", "invalid_skill_tags", "skills", id, e.Error()})
			continue
		}
		pid := PluginID("skill", id, counts[id])
		m, _ := canonical(map[string]any{"schema": "octo.legacy-backfill/v1", "source_table": "skills", "source_id": id, "display_name": display, "icon_url": icon, "description": desc})
		pkg, _ := canonical(map[string]any{"source_skill_id": source, "readme_content": readme, "file_name": fileName, "file_url": fileURL, "file_size": size, "file_sha256": sha})
		vid := DeterministicID("skillver", id+":"+v)
		if current != "" {
			vid = DeterministicID("skillver", current)
		}
		d := sql.NullTime{}
		if deleted {
			d = sql.NullTime{Time: u, Valid: true}
		}
		tj, _ := canonical(tags)
		p.plugins = append(p.plugins, plugRow{pid, n, "skill", cats["categories:"+cat], string(tj), ownerName, owner, space, visibility, creator, "human", "", "", string(m), string(pkg), hashJSON(m), both(m, pkg), vid, active(d), c, u, d})
		vs, issue, e := r.skillVersions(ctx, id, pid, v, creatorID, c, m, pkg)
		if e != nil {
			return e
		}
		p.versions = append(p.versions, vs...)
		if issue != nil {
			p.issues = append(p.issues, *issue)
		}
	}
	return rs.Err()
}
func (r *Runner) skillVersions(ctx context.Context, sid, pid, fv, fby string, fc time.Time, m, pkg []byte) ([]verRow, *Issue, error) {
	rs, e := r.db.QueryContext(ctx, `SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions WHERE skill_id=? ORDER BY created_at,id`, sid)
	if e != nil {
		return nil, nil, e
	}
	defer rs.Close()
	var out []verRow
	for rs.Next() {
		var id, v, by string
		var ch, storage sql.NullString
		var c time.Time
		if e = rs.Scan(&id, &v, &ch, &storage, &by, &c); e != nil {
			return nil, nil, e
		}
		vp := pkg
		if storage.Valid {
			var x any
			if e = json.Unmarshal([]byte(storage.String), &x); e != nil {
				return nil, nil, e
			}
			vp, _ = canonical(map[string]any{"legacy_storage": x})
		}
		if by == "" {
			by = fby
		}
		out = append(out, mkver(DeterministicID("skillver", id), pid, v, ch.String, by, c, m, vp))
	}
	if len(out) == 0 {
		if fv == "" {
			fv = "legacy"
		}
		out = append(out, mkver(DeterministicID("skillver", sid+":"+fv), pid, fv, "", fby, fc, m, pkg))
		i := Issue{"info", "synthetic_skill_version", "skills", sid, "no version rows; current snapshot created"}
		return out, &i, nil
	}
	return out, nil, rs.Err()
}

func (r *Runner) mcps(ctx context.Context, counts map[string]int, p *plan) error {
	q := `SELECT id,name,slug,slogan,category,icon,icon_version,tags_json,tools_json,usage_examples_json,faqs_json,notes_json,visibility,owner_uid,space_id,creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,transport,config_json,created_at,updated_at,deleted_at FROM mcp_servers`
	rs, e := r.db.QueryContext(ctx, q)
	if e != nil {
		return e
	}
	defer rs.Close()
	for rs.Next() {
		var id, n, slug, slogan, cat, icon, tags, tools, examples, faqs, notes, visibility, owner, creator, by, transport, config string
		var iconV int
		var space, bu, bn sql.NullString
		var c, u time.Time
		var d sql.NullTime
		if e = rs.Scan(&id, &n, &slug, &slogan, &cat, &icon, &iconV, &tags, &tools, &examples, &faqs, &notes, &visibility, &owner, &space, &creator, &by, &bu, &bn, &transport, &config, &c, &u, &d); e != nil {
			return e
		}
		safe, e := SanitizeConnectorJSON([]byte(config))
		if e != nil {
			p.issues = append(p.issues, Issue{"skip", "unsafe_connector_config", "mcp_servers", id, e.Error()})
			continue
		}
		var tv []string
		var tl, ex, fq, nt, cfg any
		if json.Unmarshal([]byte(tags), &tv) != nil || json.Unmarshal([]byte(tools), &tl) != nil || json.Unmarshal([]byte(examples), &ex) != nil || json.Unmarshal([]byte(faqs), &fq) != nil || json.Unmarshal([]byte(notes), &nt) != nil || json.Unmarshal(safe, &cfg) != nil {
			p.issues = append(p.issues, Issue{"skip", "invalid_connector_json", "mcp_servers", id, "invalid JSON column"})
			continue
		}
		m, _ := canonical(map[string]any{"schema": "octo.legacy-backfill/v1", "source_table": "mcp_servers", "source_id": id, "slug": slug, "slogan": slogan, "legacy_category": cat, "icon": icon, "icon_version": iconV})
		pkg, _ := canonical(map[string]any{"transport": transport, "config": cfg, "tools": tl, "usage_examples": ex, "faqs": fq, "notes": nt})
		pid := PluginID("connector", id, counts[id])
		vid := DeterministicID("connectorver", id)
		tj, _ := canonical(tv)
		p.plugins = append(p.plugins, plugRow{pid, n, "connector", "", string(tj), "", owner, space.String, visibility, creator, by, bu.String, bn.String, string(m), string(pkg), hashJSON(m), both(m, pkg), vid, active(d), c, u, d})
		p.versions = append(p.versions, mkver(vid, pid, "legacy", "", owner, c, m, pkg))
	}
	return rs.Err()
}

func (p plan) hash() string {
	var l []string
	for _, x := range p.cats {
		l = append(l, "c:"+x.id+":"+x.name+":"+x.types)
	}
	for _, x := range p.plugins {
		l = append(l, "p:"+x.id+":"+x.phash)
	}
	for _, x := range p.versions {
		l = append(l, "v:"+x.id+":"+x.phash)
	}
	return digestLines(l)
}
func (r *Runner) apply(ctx context.Context, p plan, rep *Report) error {
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	run := func(q string, a ...any) error {
		res, e := tx.ExecContext(ctx, q, a...)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			rep.Observed.Existing++
		} else {
			rep.Observed.Inserted++
		}
		return nil
	}
	for _, x := range p.cats {
		if e = run(`INSERT IGNORE INTO plugin_categories(category_id,name,icon_key,plugin_types_json,sort_order,status,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?)`, x.id, x.name, x.icon, x.types, x.order, x.status, x.created, x.updated, ntime(x.deleted)); e != nil {
			return e
		}
	}
	for _, x := range p.plugins {
		if e = run(`INSERT IGNORE INTO plugins(plugin_id,plugin_name,plugin_type,category_id,tags_json,publisher,owner_uid,space_id,visibility,creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,manifest_json,plugin_json,manifest_hash,plugin_hash,current_version_id,status,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.id, x.name, x.typ, nstr(x.cat), x.tags, x.publisher, x.owner, nstr(x.space), x.visibility, x.creator, x.by, nstr(x.botUID), nstr(x.botName), x.manifest, x.pkg, x.mhash, x.phash, x.versionID, x.status, x.created, x.updated, ntime(x.deleted)); e != nil {
			return e
		}
		if e = run(`INSERT IGNORE INTO plugin_audit_logs(audit_log_id,plugin_id,action,operator_id,operator_name,request_id,before_hash,after_hash,manifest_snapshot_json,plugin_snapshot_json,remark,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, DeterministicID("audit", x.id), x.id, "import", x.owner, x.creator, "historical-backfill:"+x.id, nil, x.phash, x.manifest, x.pkg, "deterministic historical backfill", x.created); e != nil {
			return e
		}
	}
	for _, x := range p.versions {
		if e = run(`INSERT IGNORE INTO plugin_versions(version_id,plugin_id,version,manifest_json,plugin_json,manifest_hash,plugin_hash,relations_json,changelog,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, x.id, x.pid, x.version, x.manifest, x.pkg, x.mhash, x.phash, "[]", nstr(x.changelog), x.by, x.created); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (r *Runner) verify(ctx context.Context, p plan, issues []Issue) (Counts, string, []Issue, error) {
	var c Counts
	var l []string
	for _, x := range p.cats {
		var n, t string
		e := r.db.QueryRowContext(ctx, `SELECT name,plugin_types_json FROM plugin_categories WHERE category_id=?`, x.id).Scan(&n, &t)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing++
			continue
		}
		if e != nil {
			return c, "", issues, e
		}
		c.Categories++
		l = append(l, "c:"+x.id+":"+n+":"+compact(t))
	}
	for _, x := range p.plugins {
		var typ, h string
		e := r.db.QueryRowContext(ctx, `SELECT plugin_type,plugin_hash FROM plugins WHERE plugin_id=?`, x.id).Scan(&typ, &h)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing += 2
			continue
		}
		if e != nil {
			return c, "", issues, e
		}
		if typ != x.typ || h != x.phash {
			c.Conflicts++
			issues = append(issues, Issue{"error", "plugin_conflict", "plugins", x.id, "type or hash differs"})
		}
		c.Plugins++
		l = append(l, "p:"+x.id+":"+h)
		var aid string
		e = r.db.QueryRowContext(ctx, `SELECT audit_log_id FROM plugin_audit_logs WHERE audit_log_id=?`, DeterministicID("audit", x.id)).Scan(&aid)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing++
		} else if e != nil {
			return c, "", issues, e
		} else {
			c.Audits++
		}
	}
	for _, x := range p.versions {
		var h string
		e := r.db.QueryRowContext(ctx, `SELECT plugin_hash FROM plugin_versions WHERE version_id=?`, x.id).Scan(&h)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing++
			continue
		}
		if e != nil {
			return c, "", issues, e
		}
		if h != x.phash {
			c.Conflicts++
			issues = append(issues, Issue{"error", "version_conflict", "plugin_versions", x.id, "hash differs"})
		}
		c.Versions++
		l = append(l, "v:"+x.id+":"+h)
	}
	return c, digestLines(l), issues, nil
}
func compact(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return s
}
