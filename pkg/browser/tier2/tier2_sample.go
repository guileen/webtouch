package tier2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
	"uws/pkg/matcher"
)

// ── HTTP 靶点 ───────────────────────────────────────────────

func newTier2Servers() (landing, cross, ref, mut *httptest.Server) {
	landing = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(tier2LandingHTML))
	}))
	crossHTML := strings.ReplaceAll(tier2CrossHTML, "__LANDING_ORIGIN__", landing.URL)
	cross = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cross.html") {
			_, _ = w.Write([]byte(crossHTML))
			return
		}
		http.NotFound(w, r)
	}))
	ref = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stable.html":
			_, _ = w.Write([]byte(tier2StableHTML))
		case "/drifting.html":
			_, _ = w.Write([]byte(tier2DriftingHTML))
		default:
			http.NotFound(w, r)
		}
	}))
	mut = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sem, mutation := parseTier2MutPath(r.URL.Path)
		if sem == "" || mutation == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(tier2MutatedHTML(sem, mutation)))
	}))
	return
}

func parseTier2MutPath(path string) (string, string) {
	const prefix = "/mut_"
	if !strings.HasPrefix(path, prefix) {
		return "", ""
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".html")
	parts := strings.Split(rest, "_")
	if len(parts) < 2 {
		return "", ""
	}
	mutation := parts[len(parts)-1]
	sem, err := url.QueryUnescape(strings.Join(parts[:len(parts)-1], "_"))
	if err != nil {
		return "", ""
	}
	return sem, mutation
}

// ── 时间系综采样 ─────────────────────────────────────────────

const tier2CollectFeatureJS = `function __uws_tier2_collectFeature(el){
  if(!el) return null;
  var ancestors=[];
  var p=el.parentElement,d=0;
  while(p && p.nodeType===1 && d<12){
    ancestors.push({tag:p.tagName.toLowerCase(),role:p.getAttribute('role')||''});
    p=p.parentElement;d++;
  }
  return {tag:el.tagName.toLowerCase(),role:el.getAttribute('role')||'',attrs:[],ancestors:ancestors};
}`

const tier2MarkerTemplate = `(function(){
  var SELECTORS=__SELECTORS__;
  for(var i=0;i<SELECTORS.length;i++){window['__uws_sel_'+i]=SELECTORS[i];}
  function collect(i){return __uws_tier2_collectFeature(document.querySelector(window['__uws_sel_'+i]));}
  window.__uws_tier2_samples={t0:{},t1:{},t2:{}};
  function snap(name){
    for(var i=0;i<SELECTORS.length;i++){window.__uws_tier2_samples[name][SELECTORS[i]]=collect(i);}
  }
  window.__uws_mark_t0=false;window.__uws_mark_t1=false;window.__uws_mark_t2=false;
  document.addEventListener('DOMContentLoaded',function(){window.__uws_mark_t0=true;snap('t0');},{once:true});
  window.addEventListener('load',function(){window.__uws_mark_t1=true;snap('t1');setTimeout(function(){window.__uws_mark_t2=true;snap('t2');},400);},{once:true});
})();`

func installTier2TemporalMarker(ctx context.Context, s Session, selectors []string) error {
	b, _ := json.Marshal(selectors)
	source := tier2CollectFeatureJS + "\n" + strings.Replace(tier2MarkerTemplate, "__SELECTORS__", string(b), 1)
	return s.CallSession(ctx, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": source}, nil)
}

func sampleTier2TemporalPage(ctx context.Context, s Session, pageURL string, selectors []string) ([]tier2TemporalRow, error) {
	if err := s.Navigate(ctx, pageURL); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(8 * time.Second)
	marked := false
	for time.Now().Before(deadline) {
		ok, _ := s.EvalBool(ctx, "window.__uws_mark_t2 === true")
		if ok {
			marked = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !marked {
		return nil, fmt.Errorf("tier2: t2 marker not ready for %s", pageURL)
	}
	var samples map[string]map[string]tier2RawFeature
	if err := s.EvalValue(ctx, "window.__uws_tier2_samples || {}", &samples); err != nil {
		return nil, err
	}
	rows := make([]tier2TemporalRow, 0, len(selectors))
	for _, selector := range selectors {
		sem := tier2SemanticForSelector(selector)
		byT := make(map[string]tier2RawFeature)
		hashes := make(map[string]uint64)
		all := true
		for _, tname := range []string{"t0", "t1", "t2"} {
			f, ok := samples[tname][selector]
			if !ok {
				all = false
				continue
			}
			byT[tname] = f
			hashes[tname] = tier2StructuralHash(f)
		}
		unique := map[uint64]bool{}
		for _, h := range hashes {
			unique[h] = true
		}
		rows = append(rows, tier2TemporalRow{
			Page:           pageURL,
			Semantic:       sem,
			Selector:       selector,
			ByT:            byT,
			Hashes:         hashes,
			Invariant:      all && len(unique) == 1,
			AncRoleChanged: tier2AncestorRoleChanged(byT),
			StructT0:       byT["t0"],
		})
	}
	return rows, nil
}

func tier2StructuralHash(f tier2RawFeature) uint64 {
	anc := make([]matcher.StructAncestor, 0, len(f.Ancestors))
	for _, a := range f.Ancestors {
		anc = append(anc, matcher.StructAncestor{Tag: a.Tag, Role: a.Role})
	}
	return matcher.StructuralSimHash64(matcher.StructFeature{
		Tag:       f.Tag,
		Role:      f.Role,
		Ancestors: anc,
	})
}

func tier2AncestorRoleChanged(byT map[string]tier2RawFeature) bool {
	roles := map[string]bool{}
	for _, f := range byT {
		for _, a := range f.Ancestors {
			if a.Role != "" && a.Role != "generic" && a.Role != "none" {
				roles[a.Role] = true
				break
			}
		}
	}
	return len(roles) > 1
}

func tier2BuildReference(rows []tier2TemporalRow) []tier2ReferenceEntry {
	seen := map[string]bool{}
	var out []tier2ReferenceEntry
	for _, r := range rows {
		if !r.Invariant || r.Semantic == "" || seen[r.Semantic] {
			continue
		}
		seen[r.Semantic] = true
		out = append(out, tier2ReferenceEntry{Semantic: r.Semantic, Struct: r.StructT0})
	}
	return out
}

func tier2Dim0FromRows(rows []tier2TemporalRow, ref []tier2ReferenceEntry) Dim0 {
	d := Dim0{Records: len(rows), ReferenceEntries: len(ref)}
	for _, r := range rows {
		if r.Invariant {
			d.Stable++
		} else {
			d.Drifted++
			d.DriftDetail = append(d.DriftDetail, DriftRow{
				Page:     r.Page,
				Semantic: r.Semantic,
				HashT0:   fmt.Sprintf("%016x", r.Hashes["t0"]),
				HashT2:   fmt.Sprintf("%016x", r.Hashes["t2"]),
			})
		}
	}
	return d
}

func tier2SemanticForSelector(selector string) string {
	for _, t := range tier2Targets {
		if t.Selector == selector {
			return t.Text
		}
	}
	return ""
}

// ── 维度1 候选采集/定位 ───────────────────────────────────────

const tier2CandidateJS = `function __tier2_feature(el){
  if(!el) return null;
  var ancestors=[];
  var p=el.parentElement,d=0;
  while(p && p.nodeType===1 && d<12){
    ancestors.push({tag:p.tagName.toLowerCase(),role:p.getAttribute('role')||''});
    p=p.parentElement;d++;
  }
  return {tag:el.tagName.toLowerCase(),role:el.getAttribute('role')||'',ancestors:ancestors};
}
function __tier2_path(el){
  if(!el||el.nodeType!==1) return '';
  var seg=[],cur=el;
  while(cur&&cur.nodeType===1&&cur!==document.documentElement){
    var part=cur.tagName.toLowerCase();
    if(cur.id){seg.unshift(part+'#'+cur.id);break;}
    var parent=cur.parentElement;
    if(parent){
      var idx=1;
      for(var sib=cur.previousElementSibling;sib;sib=sib.previousElementSibling)idx++;
      part+=':nth-of-type('+idx+')';
    }
    seg.unshift(part);cur=parent;
  }
  return seg.join(' > ');
}
function __tier2_collect(){
  var sel='button, a[href], input:not([type=hidden]), textarea, select, [role="button"], [role="link"], [onclick], [data-testid]';
  var els=document.querySelectorAll(sel);
  var out=[];
  for(var i=0;i<els.length;i++){
    var el=els[i];
    var text=(el.value||el.placeholder||el.getAttribute('aria-label')||el.getAttribute('data-testid')||el.getAttribute('title')||el.textContent||'');
    text=String(text).replace(/[\s]+/g,' ').trim();
    var feat=__tier2_feature(el);
    var r=el.getBoundingClientRect();
    out.push({text:text,tag:feat.tag,role:feat.role,ancestors:feat.ancestors,path:__tier2_path(el),truth:el.getAttribute('data-t2truth')==='1',cx:Math.round(r.left+r.width/2),cy:Math.round(r.top+r.height/2)});
  }
  return out;
}`

func collectTier2Candidates(ctx context.Context, s Session) ([]tier2Candidate, error) {
	raw, err := s.EvalResult(ctx, "JSON.stringify((function(){"+tier2CandidateJS+" return __tier2_collect();})())")
	if err != nil {
		return nil, err
	}
	var rr []tier2CandidateRaw
	if err := json.Unmarshal([]byte(raw), &rr); err != nil {
		return nil, err
	}
	out := make([]tier2Candidate, 0, len(rr))
	for _, r := range rr {
		feat := tier2RawFeature{Tag: r.Tag, Role: r.Role, Ancestors: r.Ancestors}
		sh := tier2StructuralHash(feat)
		th := matcher.SimHash64(r.Text)
		out = append(out, tier2Candidate{
			Text: r.Text, Tag: r.Tag, Role: r.Role, Path: r.Path, Truth: r.Truth,
			CX: r.CX, CY: r.CY, StructHash: sh, TextHash: th, Ancestors: r.Ancestors,
		})
	}
	return out, nil
}

func tier2RulesFromReference(ref []tier2ReferenceEntry) map[string]tier2Rule {
	out := map[string]tier2Rule{}
	for _, e := range ref {
		if e.Semantic == "" {
			continue
		}
		out[e.Semantic] = tier2Rule{
			Semantic: e.Semantic, Text: e.Semantic,
			StructHash: tier2StructuralHash(e.Struct),
			Tag:        e.Struct.Tag, Role: e.Struct.Role,
		}
	}
	return out
}

func locateTier2(cands []tier2Candidate, rule tier2Rule) *tier2Candidate {
	for i := range cands {
		cands[i].TextDist = matcher.HammingDistance(cands[i].TextHash, matcher.SimHash64(rule.Text))
		cands[i].StructDist = matcher.HammingDistance(cands[i].StructHash, rule.StructHash)
	}
	bestIdx := -1
	for i := range cands {
		textOK := cands[i].TextDist <= tier2D
		compat := matcher.StructRoleCompatible(
			matcher.StructFeature{Tag: cands[i].Tag, Role: cands[i].Role},
			matcher.StructFeature{Tag: rule.Tag, Role: rule.Role},
		)
		structOK := cands[i].StructDist <= tier2D && compat
		if !textOK && !structOK {
			continue
		}
		if rule.StructHash != 0 && cands[i].StructDist > tier2Soft && !textOK {
			continue
		}
		if bestIdx < 0 || cands[i].StructDist < cands[bestIdx].StructDist ||
			(cands[i].StructDist == cands[bestIdx].StructDist && cands[i].TextDist < cands[bestIdx].TextDist) {
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil
	}
	if !matcher.StructRoleCompatible(
		matcher.StructFeature{Tag: cands[bestIdx].Tag, Role: cands[bestIdx].Role},
		matcher.StructFeature{Tag: rule.Tag, Role: rule.Role},
	) {
		return nil
	}
	return &cands[bestIdx]
}

func tier2SelectedDist(sel *tier2Candidate, kind string) int {
	if sel == nil {
		return 999
	}
	if kind == "text" {
		return sel.TextDist
	}
	return sel.StructDist
}

func tier2UWSVerdict(sel *tier2Candidate) string {
	if sel == nil {
		return "miss"
	}
	if sel.Truth {
		return "success"
	}
	if sel.Text != "" && sel.Tag != "" {
		return "false_heal"
	}
	return "miss"
}

func tier2JudgeVerdict(sel *tier2Candidate, cands []tier2Candidate, oracle tier2Target) string {
	truthIdx := -1
	for i := range cands {
		if cands[i].Truth {
			truthIdx = i
			break
		}
	}
	if truthIdx < 0 {
		return "miss"
	}
	if sel == nil {
		return "miss"
	}
	selIdx := -1
	for i := range cands {
		if cands[i].Path == sel.Path {
			selIdx = i
			break
		}
	}
	if selIdx != truthIdx {
		if tier2TextCompatible(sel.Text, oracle.Text) && tier2RoleCompatible(*sel, oracle) {
			return "false_heal"
		}
		return "miss"
	}
	if tier2TextCompatible(sel.Text, oracle.Text) && tier2RoleCompatible(*sel, oracle) {
		return "success"
	}
	return "false_heal"
}

func tier2TextCompatible(got, oracle string) bool {
	if got == "" {
		return false
	}
	return got == oracle || strings.Contains(oracle, got) || strings.Contains(got, oracle)
}

func tier2RoleCompatible(c tier2Candidate, oracle tier2Target) bool {
	return matcher.StructRoleCompatible(
		matcher.StructFeature{Tag: c.Tag, Role: c.Role},
		matcher.StructFeature{Tag: oracle.Tag, Role: oracle.Role},
	)
}

func tier2Dim1FromRows(rows []Dim1Row) Dim1 {
	d := Dim1{TotalCases: len(rows), Rows: rows}
	var lats []float64
	for _, r := range rows {
		switch r.UWSVerdict {
		case "success":
			d.Success++
		case "false_heal":
			d.FalseHeal++
		default:
			d.Miss++
		}
		if r.Candidates > d.MaxCandidates {
			d.MaxCandidates = r.Candidates
		}
		lats = append(lats, r.LocateLatencyMS)
	}
	if len(rows) > 0 {
		d.SurvivalRate = roundTier2(float64(d.Success)/float64(len(rows)), 4)
	}
	if len(lats) > 0 {
		sum := 0.0
		for _, x := range lats {
			sum += x
		}
		d.LocateLatencyMean = roundTier2(sum/float64(len(lats)), 3)
		d.LocateLatencyP99 = roundTier2(nearestRankP99(lats), 3)
	}
	return d
}
