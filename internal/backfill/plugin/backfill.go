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
	Relations  int `json:"relations"`
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
	id, pid, version, manifest, pkg, mhash, phash, relations, changelog, by string
	created                                                                 time.Time
}
type relRow struct {
	id, source, target, typ, data, by string
	order                             int
	status                            int
	created, updated                  time.Time
	deleted                           sql.NullTime
}
type plan struct {
	cats      []catRow
	plugins   []plugRow
	relations []relRow
	versions  []verRow
	issues    []Issue
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
	rep := Report{Mode: o.Mode, StartedAt: start, Issues: p.issues, Expected: Counts{Categories: len(p.cats), Plugins: len(p.plugins), Relations: len(p.relations), Versions: len(p.versions), Audits: len(p.plugins)}, ExpectedHash: p.hash()}
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
	tags, e := r.tags(ctx, "skill_tags")
	if e != nil {
		return p, e
	}
	expertTags, e := r.tags(ctx, "expert_tags")
	if e != nil {
		return p, e
	}
	if e = r.skills(ctx, counts, cats, tags, &p); e != nil {
		return p, e
	}
	if e = r.experts(ctx, counts, cats, expertTags, &p); e != nil {
		return p, e
	}
	if e = r.squads(ctx, counts, cats, expertTags, &p); e != nil {
		return p, e
	}
	if e = r.mcps(ctx, counts, &p); e != nil {
		return p, e
	}
	if e = validatePlanReferences(p); e != nil {
		return p, fmt.Errorf("invalid generated references: %w", e)
	}
	if e = validateGraph(p.relations, 16, 500); e != nil {
		return p, fmt.Errorf("invalid generated relation graph: %w", e)
	}
	setVersionRelations(&p)
	p.issues = append(p.issues, Issue{"skip", "placements_not_migrated", "legacy catalogs", "", "no confirmed placement_code mapping"})
	sort.Slice(p.cats, func(i, j int) bool { return p.cats[i].id < p.cats[j].id })
	sort.Slice(p.plugins, func(i, j int) bool { return p.plugins[i].id < p.plugins[j].id })
	sort.Slice(p.relations, func(i, j int) bool { return p.relations[i].id < p.relations[j].id })
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
func expertVisibility(v string) string {
	if v == "public" {
		return "space"
	}
	return v
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
func (r *Runner) tags(ctx context.Context, table string) (map[int64]string, error) {
	if table != "skill_tags" && table != "expert_tags" {
		return nil, fmt.Errorf("unsupported tag table %q", table)
	}
	rs, e := r.db.QueryContext(ctx, `SELECT id,name FROM `+table)
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
	return verRow{id: id, pid: pid, version: v, manifest: string(m), pkg: string(p), mhash: hashJSON(m), phash: both(m, p), relations: "[]", changelog: ch, by: by, created: c}
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

type legacySkillRef struct {
	Name         string   `json:"name"`
	ObjectKey    string   `json:"object_key"`
	ZipObjectKey string   `json:"zip_object_key"`
	FileName     string   `json:"file_name"`
	FileSize     int64    `json:"file_size"`
	Files        []string `json:"files"`
}

type legacyMember struct {
	MemberKey   string           `json:"member_key"`
	TemplateID  string           `json:"template_id"`
	Name        string           `json:"name"`
	Role        string           `json:"role"`
	IsLeader    bool             `json:"is_leader"`
	Instruction string           `json:"instruction"`
	MCPConfig   string           `json:"mcp_config"`
	Skills      []legacySkillRef `json:"skills"`
}

func decodeArray(raw string, out any) error {
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return err
	}
	return nil
}

func appendPlugin(p *plan, x plugRow, v verRow) {
	p.plugins = append(p.plugins, x)
	p.versions = append(p.versions, v)
}

func snapshotSkill(parentID string, index, occurrence int, s legacySkillRef, owner, space, visibility, creator, by string, created, updated time.Time, deleted sql.NullTime) (plugRow, verRow) {
	raw, _ := canonical(s)
	source := "snapshot:" + hashJSON(raw)
	if s.ObjectKey != "" {
		source = "object_key:" + s.ObjectKey
	} else if s.ZipObjectKey != "" {
		source = "zip_object_key:" + s.ZipObjectKey
	}
	id := DeterministicID("snapshotskill", fmt.Sprintf("%s:%s:%06d", parentID, source, occurrence))
	manifest, _ := canonical(map[string]any{"schema": "octo.legacy-backfill/v1", "source_table": "embedded_skill_snapshot", "parent_plugin_id": parentID, "source_index": index})
	pkg, _ := canonical(map[string]any{"name": s.Name, "object_key": s.ObjectKey, "zip_object_key": s.ZipObjectKey, "file_name": s.FileName, "file_size": s.FileSize, "files": s.Files})
	vid := DeterministicID("snapshotskillver", id)
	x := plugRow{id: id, name: s.Name, typ: "skill", tags: "[]", owner: owner, space: space, visibility: visibility, creator: creator, by: by, manifest: string(manifest), pkg: string(pkg), mhash: hashJSON(manifest), phash: both(manifest, pkg), versionID: vid, status: active(deleted), created: created, updated: updated, deleted: deleted}
	return x, mkver(vid, id, "legacy", "", owner, created, manifest, pkg)
}

func snapshotOccurrences[T any](items []T, key func(T) string) []int {
	seen := map[string]int{}
	out := make([]int, len(items))
	for i, item := range items {
		k := key(item)
		out[i] = seen[k]
		seen[k]++
	}
	return out
}

func skillIdentityKey(s legacySkillRef) string {
	if s.ObjectKey != "" {
		return "object_key:" + s.ObjectKey
	}
	if s.ZipObjectKey != "" {
		return "zip_object_key:" + s.ZipObjectKey
	}
	raw, _ := canonical(s)
	return "snapshot:" + hashJSON(raw)
}

func memberIdentityKey(m legacyMember) string {
	if m.MemberKey != "" {
		return "member_key:" + m.MemberKey
	}
	raw, _ := canonical(m)
	return "snapshot:" + hashJSON(raw)
}

func relation(source, target, typ string, order int, data any, by string, created, updated time.Time, deleted sql.NullTime) relRow {
	raw, _ := canonical(data)
	return relRow{id: DeterministicID("relation", fmt.Sprintf("%s:%s:%06d:%s", source, typ, order, target)), source: source, target: target, typ: typ, order: order, data: string(raw), status: active(deleted), by: by, created: created, updated: updated, deleted: deleted}
}

func (r *Runner) experts(ctx context.Context, counts map[string]int, cats map[string]string, dict map[int64]string, p *plan) error {
	q := `SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,space_id,visibility,instruction,mcp_config,skills_json,created_at,updated_at,deleted_at FROM experts`
	rs, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var id, short, name, summary, cat, rawTags, publisher, owner, creator, by, visibility, instruction, mcp, rawSkills string
		var botUID, botName, space sql.NullString
		var created, updated time.Time
		var deleted sql.NullTime
		if err = rs.Scan(&id, &short, &name, &summary, &cat, &rawTags, &publisher, &owner, &creator, &by, &botUID, &botName, &space, &visibility, &instruction, &mcp, &rawSkills, &created, &updated, &deleted); err != nil {
			return err
		}
		tags, tagErr := namesFromTagIDs([]byte(rawTags), dict)
		var skills []legacySkillRef
		safeMCP, mcpErr := SanitizeConnectorJSON([]byte(mcp))
		if tagErr != nil || decodeArray(rawSkills, &skills) != nil || mcpErr != nil {
			detail := "invalid skills_json"
			if tagErr != nil {
				detail = tagErr.Error()
			}
			if mcpErr != nil {
				detail = mcpErr.Error()
			}
			p.issues = append(p.issues, Issue{"skip", "invalid_expert_snapshot", "experts", id, detail})
			continue
		}
		pid := PluginID("expert", id, counts[id])
		manifest, _ := canonical(map[string]any{"schema": "octo.legacy-backfill/v1", "source_table": "experts", "source_id": id, "short_name": short, "summary": summary})
		var cfg any
		_ = json.Unmarshal(safeMCP, &cfg)
		pkg, _ := canonical(map[string]any{"instruction": instruction, "mcp_config": cfg})
		tj, _ := canonical(tags)
		vid := DeterministicID("expertver", id)
		x := plugRow{pid, name, "expert", cats["expert_categories:"+cat], string(tj), publisher, owner, space.String, expertVisibility(visibility), creator, by, botUID.String, botName.String, string(manifest), string(pkg), hashJSON(manifest), both(manifest, pkg), vid, active(deleted), created, updated, deleted}
		appendPlugin(p, x, mkver(vid, pid, "legacy", "", owner, created, manifest, pkg))
		skillOccurrences := snapshotOccurrences(skills, skillIdentityKey)
		for i, skill := range skills {
			sx, sv := snapshotSkill(pid, i, skillOccurrences[i], skill, owner, space.String, expertVisibility(visibility), creator, by, created, updated, deleted)
			appendPlugin(p, sx, sv)
			p.relations = append(p.relations, relation(pid, sx.id, "expert_skill", i, map[string]any{"source_index": i}, owner, created, updated, deleted))
		}
	}
	return rs.Err()
}

func (r *Runner) squads(ctx context.Context, counts map[string]int, cats map[string]string, dict map[int64]string, p *plan) error {
	q := `SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,space_id,visibility,leader,strategies_json,dependencies_json,permission,members_json,created_at,updated_at,deleted_at FROM expert_squads`
	rs, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var id, short, name, summary, cat, rawTags, publisher, owner, creator, by, visibility, leader, strategies, dependencies, permission, rawMembers string
		var botUID, botName, space sql.NullString
		var created, updated time.Time
		var deleted sql.NullTime
		if err = rs.Scan(&id, &short, &name, &summary, &cat, &rawTags, &publisher, &owner, &creator, &by, &botUID, &botName, &space, &visibility, &leader, &strategies, &dependencies, &permission, &rawMembers, &created, &updated, &deleted); err != nil {
			return err
		}
		tags, tagErr := namesFromTagIDs([]byte(rawTags), dict)
		var members []legacyMember
		var strategyValue, dependencyValue any
		jsonErr := json.Unmarshal([]byte(strategies), &strategyValue)
		if jsonErr == nil {
			jsonErr = json.Unmarshal([]byte(dependencies), &dependencyValue)
		}
		if jsonErr == nil {
			jsonErr = decodeArray(rawMembers, &members)
		}
		if tagErr != nil || jsonErr != nil {
			detail := "invalid squad JSON"
			if tagErr != nil {
				detail = tagErr.Error()
			} else if jsonErr != nil {
				detail = jsonErr.Error()
			}
			p.issues = append(p.issues, Issue{"skip", "invalid_squad_snapshot", "expert_squads", id, detail})
			continue
		}
		valid := true
		memberMCP := make([]any, len(members))
		for i := range members {
			safe, e := SanitizeConnectorJSON([]byte(members[i].MCPConfig))
			if e != nil {
				p.issues = append(p.issues, Issue{"skip", "unsafe_member_mcp_config", "expert_squads", id, fmt.Sprintf("member %d: %v", i, e)})
				valid = false
				break
			}
			_ = json.Unmarshal(safe, &memberMCP[i])
		}
		if !valid {
			continue
		}
		pid := PluginID("expertteam", id, counts[id])
		manifest, _ := canonical(map[string]any{"schema": "octo.legacy-backfill/v1", "source_table": "expert_squads", "source_id": id, "short_name": short, "summary": summary})
		pkg, _ := canonical(map[string]any{"leader": leader, "strategies": strategyValue, "dependencies": dependencyValue, "permission": permission})
		tj, _ := canonical(tags)
		vid := DeterministicID("expertteamver", id)
		x := plugRow{pid, name, "expert_team", cats["expert_categories:"+cat], string(tj), publisher, owner, space.String, expertVisibility(visibility), creator, by, botUID.String, botName.String, string(manifest), string(pkg), hashJSON(manifest), both(manifest, pkg), vid, active(deleted), created, updated, deleted}
		appendPlugin(p, x, mkver(vid, pid, "legacy", "", owner, created, manifest, pkg))
		memberOccurrences := snapshotOccurrences(members, memberIdentityKey)
		for i, member := range members {
			memberSource := memberIdentityKey(member)
			mid := DeterministicID("snapshotmember", fmt.Sprintf("%s:%s:%06d", pid, memberSource, memberOccurrences[i]))
			mm, _ := canonical(map[string]any{"schema": "octo.legacy-backfill/v1", "source_table": "squad_member_snapshot", "parent_plugin_id": pid, "source_index": i})
			mp, _ := canonical(map[string]any{"member_key": member.MemberKey, "template_id": member.TemplateID, "role": member.Role, "is_leader": member.IsLeader, "instruction": member.Instruction, "mcp_config": memberMCP[i]})
			mvid := DeterministicID("snapshotmemberver", mid)
			mx := plugRow{mid, member.Name, "expert", cats["expert_categories:"+cat], "[]", publisher, owner, space.String, expertVisibility(visibility), creator, by, botUID.String, botName.String, string(mm), string(mp), hashJSON(mm), both(mm, mp), mvid, active(deleted), created, updated, deleted}
			appendPlugin(p, mx, mkver(mvid, mid, "legacy", "", owner, created, mm, mp))
			p.relations = append(p.relations, relation(pid, mid, "expert_team_member", i, map[string]any{"source_index": i, "member_key": member.MemberKey, "role": member.Role, "is_leader": member.IsLeader}, owner, created, updated, deleted))
			skillOccurrences := snapshotOccurrences(member.Skills, skillIdentityKey)
			for j, skill := range member.Skills {
				sx, sv := snapshotSkill(mid, j, skillOccurrences[j], skill, owner, space.String, expertVisibility(visibility), creator, by, created, updated, deleted)
				appendPlugin(p, sx, sv)
				p.relations = append(p.relations, relation(mid, sx.id, "expert_skill", j, map[string]any{"source_index": j}, owner, created, updated, deleted))
			}
		}
	}
	return rs.Err()
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
		p.plugins = append(p.plugins, plugRow{pid, n, "connector", "", string(tj), "", owner, space.String, expertVisibility(visibility), creator, by, bu.String, bn.String, string(m), string(pkg), hashJSON(m), both(m, pkg), vid, active(d), c, u, d})
		p.versions = append(p.versions, mkver(vid, pid, "legacy", "", owner, c, m, pkg))
	}
	return rs.Err()
}

func validatePlanReferences(p plan) error {
	plugins := make(map[string]struct{}, len(p.plugins))
	categories := make(map[string]struct{}, len(p.cats))
	versions := make(map[string]string, len(p.versions))
	for _, category := range p.cats {
		categories[category.id] = struct{}{}
	}
	for _, plugin := range p.plugins {
		if _, exists := plugins[plugin.id]; exists {
			return fmt.Errorf("duplicate plugin id %q", plugin.id)
		}
		plugins[plugin.id] = struct{}{}
		if plugin.cat != "" {
			if _, exists := categories[plugin.cat]; !exists {
				return fmt.Errorf("plugin %q references missing category %q", plugin.id, plugin.cat)
			}
		}
	}
	for _, relation := range p.relations {
		if _, exists := plugins[relation.source]; !exists {
			return fmt.Errorf("relation %q references missing source plugin %q", relation.id, relation.source)
		}
		if _, exists := plugins[relation.target]; !exists {
			return fmt.Errorf("relation %q references missing target plugin %q", relation.id, relation.target)
		}
	}
	for _, version := range p.versions {
		if _, exists := plugins[version.pid]; !exists {
			return fmt.Errorf("version %q references missing plugin %q", version.id, version.pid)
		}
		versions[version.id] = version.pid
	}
	for _, plugin := range p.plugins {
		if versions[plugin.versionID] != plugin.id {
			return fmt.Errorf("plugin %q references missing current version %q", plugin.id, plugin.versionID)
		}
	}
	return nil
}

func validateGraph(relations []relRow, maxDepth, maxNodes int) error {
	nodes := map[string]bool{}
	adj := map[string][]string{}
	undirected := map[string][]string{}
	for _, rel := range relations {
		if rel.source == rel.target {
			return fmt.Errorf("self-cycle at %s", rel.source)
		}
		nodes[rel.source], nodes[rel.target] = true, true
		adj[rel.source] = append(adj[rel.source], rel.target)
		undirected[rel.source] = append(undirected[rel.source], rel.target)
		undirected[rel.target] = append(undirected[rel.target], rel.source)
	}
	keys := make([]string, 0, len(nodes))
	for node := range nodes {
		keys = append(keys, node)
		sort.Strings(adj[node])
		sort.Strings(undirected[node])
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	for _, root := range keys {
		if seen[root] {
			continue
		}
		queue, count := []string{root}, 0
		seen[root] = true
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			count++
			for _, next := range undirected[n] {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		if count > maxNodes {
			return fmt.Errorf("component node count %d exceeds %d", count, maxNodes)
		}
	}
	state, longest := map[string]uint8{}, map[string]int{}
	var visit func(string) (int, error)
	visit = func(node string) (int, error) {
		if state[node] == 1 {
			return 0, fmt.Errorf("cycle at %s", node)
		}
		if state[node] == 2 {
			return longest[node], nil
		}
		state[node] = 1
		best := 1
		for _, target := range adj[node] {
			d, err := visit(target)
			if err != nil {
				return 0, err
			}
			if d+1 > best {
				best = d + 1
			}
		}
		state[node], longest[node] = 2, best
		if best > maxDepth {
			return 0, fmt.Errorf("depth %d exceeds %d at %s", best, maxDepth, node)
		}
		return best, nil
	}
	for _, node := range keys {
		if _, err := visit(node); err != nil {
			return err
		}
	}
	return nil
}

func setVersionRelations(p *plan) {
	bySourceRows := map[string][]relRow{}
	for _, rel := range p.relations {
		bySourceRows[rel.source] = append(bySourceRows[rel.source], rel)
	}
	bySource := map[string][]map[string]any{}
	for source, rows := range bySourceRows {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].order != rows[j].order {
				return rows[i].order < rows[j].order
			}
			return rows[i].id < rows[j].id
		})
		for _, rel := range rows {
			var data any
			_ = json.Unmarshal([]byte(rel.data), &data)
			bySource[source] = append(bySource[source], map[string]any{"relation_id": rel.id, "target_plugin_id": rel.target, "relation_type": rel.typ, "sort_order": rel.order, "relation": data})
		}
	}
	for i := range p.versions {
		rels := bySource[p.versions[i].pid]
		if rels == nil {
			rels = []map[string]any{}
		}
		raw, _ := canonical(rels)
		p.versions[i].relations = string(raw)
	}
}

func (p plan) hash() string {
	var l []string
	for _, x := range p.cats {
		l = append(l, "c:"+x.id+":"+x.name+":"+compact(x.types))
	}
	for _, x := range p.plugins {
		l = append(l, "p:"+x.id+":"+x.phash)
	}
	for _, x := range p.relations {
		l = append(l, fmt.Sprintf("r:%s:%s:%s:%s:%d:%s", x.id, x.source, x.target, x.typ, x.order, compact(x.data)))
	}
	for _, x := range p.versions {
		l = append(l, "v:"+x.id+":"+x.phash+":"+compact(x.relations))
	}
	return digestLines(l)
}
func applyRow(ctx context.Context, tx *sql.Tx, rep *Report, table, keyColumn string, key any, exactQuery, insertQuery string, exactArgs, insertArgs []any) error {
	var exact int
	if err := tx.QueryRowContext(ctx, exactQuery, exactArgs...).Scan(&exact); err != nil {
		return err
	}
	if exact == 1 {
		rep.Observed.Existing++
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+keyColumn+`=?`, key).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return fmt.Errorf("backfill conflict: %s %v differs from deterministic plan", table, key)
	}
	if _, err := tx.ExecContext(ctx, insertQuery, insertArgs...); err != nil {
		return err
	}
	rep.Observed.Inserted++
	return nil
}

func (r *Runner) apply(ctx context.Context, p plan, rep *Report) error {
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, x := range p.cats {
		a := []any{x.id, x.name, x.icon, x.types, x.order, x.status, x.created, x.updated, ntime(x.deleted)}
		e = applyRow(ctx, tx, rep, "plugin_categories", "category_id", x.id, `SELECT COUNT(*) FROM plugin_categories WHERE category_id=? AND name<=>? AND icon_key<=>? AND plugin_types_json<=>CAST(? AS JSON) AND sort_order<=>? AND status<=>? AND created_at<=>? AND updated_at<=>? AND deleted_at<=>?`, `INSERT INTO plugin_categories(category_id,name,icon_key,plugin_types_json,sort_order,status,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?)`, a, a)
		if e != nil {
			return e
		}
	}
	for _, x := range p.plugins {
		a := []any{x.id, x.name, x.typ, nstr(x.cat), x.tags, x.publisher, x.owner, nstr(x.space), x.visibility, x.creator, x.by, nstr(x.botUID), nstr(x.botName), x.manifest, x.pkg, x.mhash, x.phash, x.versionID, x.status, x.created, x.updated, ntime(x.deleted)}
		e = applyRow(ctx, tx, rep, "plugins", "plugin_id", x.id, `SELECT COUNT(*) FROM plugins WHERE plugin_id=? AND plugin_name<=>? AND plugin_type<=>? AND category_id<=>? AND tags_json<=>CAST(? AS JSON) AND publisher<=>? AND owner_uid<=>? AND space_id<=>? AND visibility<=>? AND creator_name<=>? AND created_by_type<=>? AND created_by_bot_uid<=>? AND created_by_bot_name<=>? AND manifest_json<=>CAST(? AS JSON) AND plugin_json<=>CAST(? AS JSON) AND manifest_hash<=>? AND plugin_hash<=>? AND current_version_id<=>? AND status<=>? AND created_at<=>? AND updated_at<=>? AND deleted_at<=>?`, `INSERT INTO plugins(plugin_id,plugin_name,plugin_type,category_id,tags_json,publisher,owner_uid,space_id,visibility,creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,manifest_json,plugin_json,manifest_hash,plugin_hash,current_version_id,status,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a, a)
		if e != nil {
			return e
		}
		aid := DeterministicID("audit", x.id)
		aa := []any{aid, x.id, "import", x.owner, x.creator, "historical-backfill:" + x.id, nil, x.phash, x.manifest, x.pkg, "deterministic historical backfill", x.created}
		e = applyRow(ctx, tx, rep, "plugin_audit_logs", "audit_log_id", aid, `SELECT COUNT(*) FROM plugin_audit_logs WHERE audit_log_id=? AND plugin_id<=>? AND action<=>? AND operator_id<=>? AND operator_name<=>? AND request_id<=>? AND before_hash<=>? AND after_hash<=>? AND manifest_snapshot_json<=>CAST(? AS JSON) AND plugin_snapshot_json<=>CAST(? AS JSON) AND remark<=>? AND created_at<=>?`, `INSERT INTO plugin_audit_logs(audit_log_id,plugin_id,action,operator_id,operator_name,request_id,before_hash,after_hash,manifest_snapshot_json,plugin_snapshot_json,remark,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, aa, aa)
		if e != nil {
			return e
		}
	}
	for _, x := range p.relations {
		a := []any{x.id, x.source, x.target, x.typ, x.order, x.data, x.status, x.by, x.created, x.updated, ntime(x.deleted)}
		e = applyRow(ctx, tx, rep, "plugin_relations", "relation_id", x.id, `SELECT COUNT(*) FROM plugin_relations WHERE relation_id=? AND source_plugin_id<=>? AND target_plugin_id<=>? AND relation_type<=>? AND sort_order<=>? AND relation_json<=>CAST(? AS JSON) AND status<=>? AND created_by<=>? AND created_at<=>? AND updated_at<=>? AND deleted_at<=>?`, `INSERT INTO plugin_relations(relation_id,source_plugin_id,target_plugin_id,relation_type,sort_order,relation_json,status,created_by,created_at,updated_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a, a)
		if e != nil {
			return e
		}
	}
	for _, x := range p.versions {
		a := []any{x.id, x.pid, x.version, x.manifest, x.pkg, x.mhash, x.phash, x.relations, nstr(x.changelog), x.by, x.created}
		e = applyRow(ctx, tx, rep, "plugin_versions", "version_id", x.id, `SELECT COUNT(*) FROM plugin_versions WHERE version_id=? AND plugin_id<=>? AND version<=>? AND manifest_json<=>CAST(? AS JSON) AND plugin_json<=>CAST(? AS JSON) AND manifest_hash<=>? AND plugin_hash<=>? AND relations_json<=>CAST(? AS JSON) AND changelog<=>? AND created_by<=>? AND created_at<=>?`, `INSERT INTO plugin_versions(version_id,plugin_id,version,manifest_json,plugin_json,manifest_hash,plugin_hash,relations_json,changelog,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a, a)
		if e != nil {
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
		var pid, action, operator, operatorName, requestID, after, manifest, pkg string
		e = r.db.QueryRowContext(ctx, `SELECT plugin_id,action,operator_id,operator_name,request_id,after_hash,manifest_snapshot_json,plugin_snapshot_json FROM plugin_audit_logs WHERE audit_log_id=?`, DeterministicID("audit", x.id)).Scan(&pid, &action, &operator, &operatorName, &requestID, &after, &manifest, &pkg)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing++
		} else if e != nil {
			return c, "", issues, e
		} else {
			if pid != x.id || action != "import" || operator != x.owner || operatorName != x.creator || requestID != "historical-backfill:"+x.id || after != x.phash || compact(manifest) != compact(x.manifest) || compact(pkg) != compact(x.pkg) {
				c.Conflicts++
				issues = append(issues, Issue{"error", "audit_conflict", "plugin_audit_logs", DeterministicID("audit", x.id), "identity, provenance, hash, or snapshot differs"})
			}
			c.Audits++
		}
	}
	for _, x := range p.relations {
		var source, target, typ, data string
		var order int
		e := r.db.QueryRowContext(ctx, `SELECT source_plugin_id,target_plugin_id,relation_type,sort_order,relation_json FROM plugin_relations WHERE relation_id=?`, x.id).Scan(&source, &target, &typ, &order, &data)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing++
			continue
		}
		if e != nil {
			return c, "", issues, e
		}
		if source != x.source || target != x.target || typ != x.typ || order != x.order || compact(data) != compact(x.data) {
			c.Conflicts++
			issues = append(issues, Issue{"error", "relation_conflict", "plugin_relations", x.id, "endpoints, type, order, or payload differs"})
		}
		c.Relations++
		l = append(l, fmt.Sprintf("r:%s:%s:%s:%s:%d:%s", x.id, source, target, typ, order, compact(data)))
	}
	for _, x := range p.versions {
		var h, relations string
		e := r.db.QueryRowContext(ctx, `SELECT plugin_hash,relations_json FROM plugin_versions WHERE version_id=?`, x.id).Scan(&h, &relations)
		if errors.Is(e, sql.ErrNoRows) {
			c.Missing++
			continue
		}
		if e != nil {
			return c, "", issues, e
		}
		if h != x.phash || compact(relations) != compact(x.relations) {
			c.Conflicts++
			issues = append(issues, Issue{"error", "version_conflict", "plugin_versions", x.id, "hash or relation snapshot differs"})
		}
		c.Versions++
		l = append(l, "v:"+x.id+":"+h+":"+compact(relations))
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
