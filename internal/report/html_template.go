package report

import (
	"html/template"
	"strings"
)

// htmlTmpl renders htmlVM into a self-contained dark dashboard. No JS and no
// external scripts; the two web fonts are BUNDLED and inlined as base64 @font-face
// (see fonts.go), so the report renders in IBM Plex Sans / JetBrains Mono with no
// network call — nothing leaves the machine. Every icon is an inline SVG, so the
// file is fully offline. Per-element colors are applied via CSS custom properties
// set by a class from a fixed name set (e.g. .t-claude{--tc:…}), never by
// interpolating a color into a style attribute, so html/template's CSS sanitizer
// can't blank them out.
//
// The @font-face <style> is spliced in (as static template text, not an action, so
// html/template never sanitizes the base64) where fontFacePlaceholder sits in the
// <head>.
var htmlTmpl = template.Must(template.New("report").Parse(
	strings.Replace(htmlSource, fontFacePlaceholder, fontFaceStyle(), 1)))

// fontFacePlaceholder marks where the bundled @font-face block is injected.
const fontFacePlaceholder = "<!--BLAMELY_FONTS-->"

const htmlSource = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<title>Blamely · {{.ShortHash}}</title>
<!--BLAMELY_FONTS-->
<style>
:root{
  --acc:#10b981; --acc2:#34d399; --red:#f0717f; --amber:#e0a33a; --blue:#5aa2f5; --cyan:#56c5d0; --violet:#bd93f5;
  --bg:#171718; --bg2:#121213; --card1:#232325; --card2:#1d1d1f; --border:#2e2e31; --border2:#39393d;
  --chip:#27272a; --foot:#161617; --text:#e3e3e6; --dim:#9b9ba2; --faint:#6b6b72; --track:#2a2a2d;
  --sans:'IBM Plex Sans',system-ui,-apple-system,Segoe UI,sans-serif;
  --mono:'JetBrains Mono',ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
}
*{box-sizing:border-box}
html,body{margin:0;padding:0}
body{
  font-family:var(--sans);color:var(--text);-webkit-font-smoothing:antialiased;
  padding:34px 20px;display:flex;justify-content:center;
  background:
    radial-gradient(1100px 520px at 18% -8%, rgba(16,185,129,.10), transparent 60%),
    radial-gradient(900px 480px at 100% 0%, rgba(90,162,245,.08), transparent 55%),
    var(--bg2);
}
.mono{font-family:var(--mono);font-variant-ligatures:none}
.ic{display:inline-flex;width:14px;height:14px;color:var(--dim);flex-shrink:0}
.ic svg{width:100%;height:100%;stroke:currentColor;fill:none;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round}
.panel{
  width:100%;max-width:1180px;background:var(--bg);
  border:1px solid var(--border);border-radius:18px;overflow:hidden;
  box-shadow:0 1px 0 rgba(255,255,255,.04) inset, 0 30px 80px -28px rgba(0,0,0,.75);
}
/* toolbar */
.bar{display:flex;align-items:center;gap:14px;padding:0 22px;height:60px;border-bottom:1px solid var(--border);
  background:linear-gradient(180deg, rgba(255,255,255,.025), transparent)}
.brand{display:flex;align-items:center;gap:10px}
.mark{width:28px;height:28px;border-radius:9px;display:flex;align-items:center;justify-content:center;
  background:linear-gradient(145deg, var(--acc2), var(--acc));color:#06281d;
  box-shadow:0 4px 14px -4px rgba(16,185,129,.6)}
.mark svg{width:15px;height:15px;stroke:currentColor;fill:none;stroke-width:2.1;stroke-linecap:round}
.brand b{font-size:14.5px;font-weight:700;letter-spacing:.01em}
.brand .sub{font-size:11px;color:var(--faint);font-weight:500}
.vsep{width:1px;height:20px;background:var(--border)}
.msg{font-size:13px;font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:40ch}
.chip{display:inline-flex;align-items:center;gap:5px;padding:3px 9px;border-radius:7px;background:var(--chip);border:1px solid var(--border);font-size:11.5px;color:var(--dim)}
.meta{margin-left:auto;display:flex;align-items:center;gap:16px;color:var(--dim);font-size:11.5px}
.meta .i{display:inline-flex;align-items:center;gap:6px}
/* grid */
.body{padding:18px;display:flex;flex-direction:column;gap:16px}
.row3{display:grid;grid-template-columns:minmax(0,1.15fr) minmax(0,.85fr) minmax(0,1.25fr);gap:16px}
.row2{display:grid;grid-template-columns:minmax(0,1.3fr) minmax(0,1fr);gap:16px}
@media(max-width:880px){.row3,.row2{grid-template-columns:1fr}}
.card{background:linear-gradient(180deg,var(--card1),var(--card2));border:1px solid var(--border);border-radius:13px;
  padding:15px 17px 17px;display:flex;flex-direction:column;min-width:0;
  box-shadow:0 1px 0 rgba(255,255,255,.035) inset}
.ct{display:flex;align-items:center;justify-content:space-between;margin-bottom:14px}
.ct .l{display:flex;align-items:center;gap:8px}
.dot{width:6px;height:6px;border-radius:2px;background:var(--acc);box-shadow:0 0 8px rgba(16,185,129,.7)}
.ct .t{font-size:11px;font-weight:600;letter-spacing:.11em;text-transform:uppercase;color:var(--dim)}
.ct .r{font-size:11px;color:var(--faint)}
/* donut */
.donut{display:flex;align-items:center;gap:18px}
.donut .wrap{position:relative;width:134px;height:134px;flex-shrink:0}
.donut .ctr{position:absolute;inset:0;display:flex;flex-direction:column;align-items:center;justify-content:center}
.donut .pct{font-size:28px;font-weight:700;line-height:1}
.donut .cap{font-size:10.5px;color:var(--dim);margin-top:4px;letter-spacing:.04em}
.leg{display:flex;flex-direction:column;gap:11px;min-width:0}
.lg{display:flex;align-items:center;gap:8px}
.lg .sw{width:9px;height:9px;border-radius:3px;flex-shrink:0}
.lg .lb{font-size:12.5px;font-weight:500;min-width:48px}
.lg .vl{font-size:12px;color:var(--dim)}
.modelchip{display:inline-flex;align-items:center;gap:6px;padding:3px 9px;border-radius:7px;
  background:color-mix(in srgb,var(--acc) 14%,transparent);border:1px solid color-mix(in srgb,var(--acc) 35%,transparent);
  font-size:11.5px;color:var(--acc2);font-weight:600;white-space:nowrap;margin-top:3px;width:fit-content}
.modelchip svg{width:11px;height:11px;fill:currentColor}
/* changes */
.big{display:flex;align-items:baseline;gap:18px}
.stat{display:flex;flex-direction:column}
.stat .n{font-size:30px;font-weight:700;line-height:1}
.stat .k{font-size:10.5px;color:var(--dim);margin-top:6px}
.stat.net{margin-left:auto;align-items:flex-end}
.stat.net .n{font-size:22px;color:var(--text)}
.split{display:flex;height:8px;border-radius:5px;overflow:hidden;background:var(--track);margin:15px 0 13px}
.split .s-add{background:linear-gradient(90deg,var(--acc),var(--acc2))}
.split .s-del{background:linear-gradient(90deg,#c8505f,var(--red))}
.tags{display:flex;gap:16px}
.tags+.tags{margin-top:9px}
.tag{display:flex;align-items:center;gap:6px;font-size:12px;color:var(--dim)}
.tag .sw{width:7px;height:7px;border-radius:2px}
.tag .vl{font-size:12px;color:var(--text);font-weight:600}
.sw-acc{background:var(--acc)} .sw-amber{background:var(--blue)} .sw-red{background:var(--red)} .sw-faint{background:var(--faint)}
/* generation */
.gen{display:flex;flex-direction:column;gap:12px}
.gr{display:flex;align-items:center;gap:12px}
.gr .lb{font-size:12.5px;width:82px;font-weight:500}
.gr .tr{flex:1;height:18px;background:var(--track);border-radius:6px;overflow:hidden}
.gr .fl{display:block;height:100%;border-radius:6px}
.gr .v{font-size:12.5px;width:26px;text-align:right;font-weight:600}
.gr .p{font-size:11.5px;width:46px;text-align:right;color:var(--dim)}
.gr.zero .lb,.gr.zero .v,.gr.zero .p{color:var(--faint)}
.g-chat{background:linear-gradient(90deg,var(--acc),var(--acc2))}
.g-cli{background:linear-gradient(90deg,#3f9aa4,var(--cyan))}
.g-completion{background:linear-gradient(90deg,#9d6fe0,var(--violet))}
.g-human{background:linear-gradient(90deg,#3f7fd4,var(--blue))}
.acc{display:flex;align-items:center;gap:8px;margin-top:4px;font-size:11.5px;color:var(--dim)}
.acc b{color:var(--acc2)}
/* tools */
.t-claude{--tc:#d97757}.t-cursor{--tc:#cdd3da}.t-codex{--tc:#19c37d}.t-copilot{--tc:#a371f7}.t-gemini{--tc:#5aa2f5}.t-devin{--tc:#4b8dd6}
.tlist{display:flex;flex-direction:column;gap:15px}
.trow{display:flex;gap:13px;align-items:flex-start}
.tav{width:36px;height:36px;flex-shrink:0;border-radius:10px;display:flex;align-items:center;justify-content:center;
  color:var(--tc);background:color-mix(in srgb,var(--tc) 15%,transparent);
  border:1px solid color-mix(in srgb,var(--tc) 40%,transparent);
  box-shadow:0 5px 16px -8px var(--tc)}
.tav svg{width:18px;height:18px}
.tbody{flex:1;min-width:0;display:flex;flex-direction:column;gap:8px}
.ttop{display:flex;align-items:center;gap:10px}
.tnm{font-size:13.5px;font-weight:600;text-transform:capitalize}
.tmodel{font-size:11px;color:var(--dim);padding:2px 8px;border:1px solid var(--border);border-radius:6px;background:var(--chip);
  max-width:24ch;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.tspc{flex:1}
.tln{font-size:13.5px;font-weight:600}.tln .tk{color:var(--faint);font-weight:400}.tln .tdel{color:var(--red);font-weight:600}
.tbar{height:7px;border-radius:5px;background:var(--track);overflow:hidden}
.tfl{display:block;height:100%;border-radius:5px;background:linear-gradient(90deg,color-mix(in srgb,var(--tc) 65%,#000),var(--tc))}
.tmeta{display:flex;align-items:center;gap:14px;font-size:11px;color:var(--dim);flex-wrap:wrap}
.tpill{color:var(--tc);background:color-mix(in srgb,var(--tc) 13%,transparent);padding:2px 8px;border-radius:6px;font-weight:600}
.ttok .ar{color:var(--faint);font-weight:600}.ttok .tdim{color:var(--faint)}
/* by_tool legend tooltip (pure CSS, no JS) */
.tip{position:relative;display:inline-flex;align-items:center}
.tipq{width:13px;height:13px;border-radius:50%;border:1px solid var(--border2);color:var(--faint);
  font-size:9px;font-weight:700;line-height:1;display:inline-flex;align-items:center;justify-content:center;
  cursor:help;font-family:var(--sans);font-style:normal}
.tip:hover .tipq{color:var(--dim);border-color:var(--dim)}
.tipbox{position:absolute;top:calc(100% + 9px);left:0;width:250px;padding:12px 13px;border-radius:11px;
  background:var(--bg);border:1px solid var(--border2);box-shadow:0 14px 38px -10px rgba(0,0,0,.82);z-index:30;
  opacity:0;visibility:hidden;pointer-events:none;transform:translateY(-4px);transition:opacity .14s ease,transform .14s ease}
.tip:hover .tipbox{opacity:1;visibility:visible;transform:translateY(0)}
.tipbox .th{font-size:11px;color:var(--dim);letter-spacing:.02em;margin-bottom:10px}
.tiprow{display:flex;align-items:flex-start;gap:9px;font-size:11.5px;line-height:1.4}
.tiprow+.tiprow{margin-top:9px}
.tiprow .sw{width:9px;height:9px;border-radius:3px;flex-shrink:0;margin-top:3px}
.tiprow b{font-weight:600;color:var(--text)}.tiprow span{color:var(--dim)}
.sw-ai{background:linear-gradient(135deg,var(--acc2),var(--acc))}
.sw-human{background:var(--blue)}
.sw-paste{background:var(--amber)}
/* files */
.frow{display:flex;align-items:center;gap:10px;padding:12px 4px;border-top:1px solid var(--border)}
.frow:first-child{border-top:none}
.frow .nm{font-size:12.5px;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.fattr{display:inline-flex;align-items:center;gap:5px;padding:2px 8px;border-radius:6px;background:color-mix(in srgb,var(--acc) 13%,transparent);font-size:11px;color:var(--acc2)}
.fattr .sw{width:6px;height:6px;border-radius:2px;background:var(--acc)}
.fdash{font-size:11px;color:var(--faint)}
.fadd{font-size:12px;color:var(--acc2);width:40px;text-align:right}
.fdel{font-size:12px;width:36px;text-align:right;color:var(--faint)}
.fdel.on{color:var(--red)}
.fbar{display:flex;height:6px;width:64px;border-radius:3px;overflow:hidden;background:var(--track)}
.fbar .s-add{background:var(--acc)} .fbar .s-del{background:var(--red)}
/* leaderboard */
.lb-l{display:flex;flex-direction:column;gap:15px}
.lr{display:flex;align-items:center;gap:12px}
.lr .rk{font-size:12px;color:var(--faint);width:14px}
.lr .av{width:32px;height:32px;flex-shrink:0;display:flex;align-items:center;justify-content:center;font-size:13px;font-weight:600}
.lr .av svg{width:15px;height:15px}
.av.l-model{color:var(--acc2);background:color-mix(in srgb,var(--acc) 16%,transparent);border:1px solid color-mix(in srgb,var(--acc) 42%,transparent);border-radius:9px}
.av.l-human{color:var(--blue);background:color-mix(in srgb,var(--blue) 16%,transparent);border:1px solid color-mix(in srgb,var(--blue) 42%,transparent);border-radius:50%}
.lr .col{flex:1;min-width:0;display:flex;flex-direction:column;gap:6px}
.lr .top{display:flex;align-items:center;justify-content:space-between;gap:8px}
.lr .nm{font-size:12.5px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.lr .ln{font-size:12.5px;font-weight:600}
.lr .pc{font-size:11px;color:var(--dim);width:44px;text-align:right}
.lr .tr{height:6px;background:var(--track);border-radius:3px;overflow:hidden}
.lr .fl{display:block;height:100%;border-radius:3px}
.fl.l-model{background:linear-gradient(90deg,var(--acc),var(--acc2))} .fl.l-human{background:linear-gradient(90deg,#3f7fd4,var(--blue))}
.lfoot{font-size:11px;color:var(--faint);margin-top:2px}
/* file ranges */
.ranges .rfile{display:flex;align-items:center;gap:9px;margin-top:16px;padding-bottom:10px;border-bottom:1px solid var(--border)}
.ranges .rfile:first-child{margin-top:2px}
.ranges .rfile .rname{flex:1;font-size:12.5px;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.ranges .rrow{display:flex;align-items:center;gap:14px;padding:6px 0 6px 24px}
.ranges .rrow:first-of-type{padding-top:10px}
.ranges .rloc{font-size:11.5px;color:var(--dim);width:80px}
.ranges .rkind{font-size:11px;color:var(--faint);width:50px}
.ranges .rattr{font-size:11.5px;color:var(--text)}
.ranges .rattr.ai{color:var(--acc2)}
.ranges .rattr .sub{color:var(--dim)}
.ranges .rattr.hum{color:var(--blue)}
/* footer */
.foot{display:flex;align-items:center;gap:20px;padding:0 22px;height:48px;border-top:1px solid var(--border);background:var(--foot);font-size:11.5px}
.foot .ic{width:13px;height:13px}
.foot .h{font-size:11px;font-weight:600;letter-spacing:.09em;text-transform:uppercase;color:var(--dim)}
.foot .kv{display:flex;align-items:center;gap:6px}
.foot .kv .k{color:var(--faint)}
.foot .kv .v{font-weight:600}
.foot .flow{margin-left:auto;color:var(--faint)}
.ver{padding:11px 22px 16px;font-size:11px;color:var(--faint);text-align:right}
</style>
</head>
<body>
<div class="panel">
  <div class="bar">
    <div class="brand">
      <span class="mark"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3.2"/><path d="M12 4.5v3M12 16.5v3M4.5 12h3M16.5 12h3"/></svg></span>
      <b>Blamely</b><span class="sub">report</span>
    </div>
    <span class="vsep"></span>
    <span class="msg">{{if .Subject}}&ldquo;{{.Subject}}&rdquo;{{end}}</span>
    <span class="chip mono"><span class="ic"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><line x1="3" y1="12" x2="9" y2="12"/><line x1="15" y1="12" x2="21" y2="12"/></svg></span>{{.ShortHash}}</span>
    <div class="meta">
      {{if .Branch}}<span class="i"><span class="ic"><svg viewBox="0 0 24 24"><line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/></svg></span><span class="mono">{{.Branch}}</span></span>{{end}}
      {{if .Author}}<span class="i"><span class="ic"><svg viewBox="0 0 24 24"><path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg></span><span class="mono">{{.Author}}</span></span>{{end}}
      {{if .Ago}}<span>{{.Ago}}</span>{{end}}
    </div>
  </div>

  <div class="body">
    <div class="row3">
      <div class="card">
        <div class="ct"><div class="l"><span class="dot"></span><span class="t">Attribution</span></div></div>
        <div class="donut">
          <div class="wrap">
            <svg width="134" height="134" viewBox="0 0 134 134" style="transform:rotate(-90deg)">
              <circle cx="67" cy="67" r="52" fill="none" stroke="var(--track)" stroke-width="13"/>
              <circle cx="67" cy="67" r="52" fill="none" stroke="var(--blue)" stroke-width="13" stroke-dasharray="{{.DonutHumanDash}}" stroke-dashoffset="{{.DonutHumanOff}}" stroke-linecap="round"/>
              <circle cx="67" cy="67" r="52" fill="none" stroke="var(--acc)" stroke-width="13" stroke-dasharray="{{.DonutAIDash}}" stroke-dashoffset="0" stroke-linecap="round"/>
            </svg>
            <div class="ctr"><span class="pct mono">{{.AIPct}}%</span><span class="cap">AI authored</span></div>
          </div>
          <div class="leg">
            <div class="lg"><span class="sw sw-acc"></span><span class="lb">AI</span><span class="vl mono">{{.AILines}} lines</span></div>
            <div class="lg"><span class="sw sw-amber"></span><span class="lb">Human</span><span class="vl mono">{{.HumanLines}} lines</span></div>
            {{if .TopModel}}<span class="modelchip mono"><svg viewBox="0 0 24 24"><path d="M12 1.6l1.7 6.3a2 2 0 0 0 1.4 1.4L21.4 11l-6.3 1.7a2 2 0 0 0-1.4 1.4L12 20.4l-1.7-6.3a2 2 0 0 0-1.4-1.4L2.6 11l6.3-1.7a2 2 0 0 0 1.4-1.4z"/></svg>{{.TopModel}}</span>{{end}}
          </div>
        </div>
      </div>

      <div class="card">
        <div class="ct"><div class="l"><span class="dot"></span><span class="t">Changes</span></div></div>
        <div class="big">
          <div class="stat"><span class="n mono" style="color:var(--acc2)">+{{.Added}}</span><span class="k">added</span></div>
          <div class="stat"><span class="n mono" style="color:var(--red)">&minus;{{.Deleted}}</span><span class="k">deleted</span></div>
          <div class="stat net"><span class="n mono">{{.Net}}</span><span class="k">net</span></div>
        </div>
        <div class="split"><span class="s-add" style="width:{{.AddedPct}}%"></span><span class="s-del" style="width:{{.DeletedPct}}%"></span></div>
        <div class="tags">
          <span class="tag"><span class="sw sw-acc"></span>AI added<span class="vl mono">{{.AIAdded}}</span></span>
          <span class="tag"><span class="sw sw-amber"></span>Human added<span class="vl mono">{{.HumanAdded}}</span></span>
        </div>
        <div class="tags">
          <span class="tag"><span class="sw sw-red"></span>AI deleted<span class="vl mono">{{.AIDeleted}}</span></span>
          <span class="tag"><span class="sw sw-faint"></span>Human deleted<span class="vl mono">{{.HumanDeleted}}</span></span>
        </div>
      </div>

      <div class="card">
        <div class="ct"><div class="l"><span class="dot"></span><span class="t">Generation</span></div></div>
        <div class="gen">
          {{range .Gen}}
          <div class="gr{{if .Zero}} zero{{end}}">
            <span class="lb">{{.Label}}</span>
            <span class="tr">{{if not .Zero}}<span class="fl g-{{.Label}}" style="width:{{.WidthPct}}%"></span>{{end}}</span>
            <span class="v mono">{{.Value}}</span><span class="p mono">{{.Pct}}%</span>
          </div>
          {{end}}
          {{if .Accepted}}<div class="acc">accepted <b class="mono">{{.Accepted.Pct}}%</b> · <span class="mono">{{.Accepted.Suggested}} suggested → {{.Accepted.Kept}} kept</span></div>{{end}}
        </div>
      </div>
    </div>

    {{if .Tools}}
    <div class="card">
      <div class="ct"><div class="l"><span class="dot"></span><span class="t">Tools</span><span class="tip"><span class="tipq">i</span><span class="tipbox"><span class="th">How blamely attributes each line</span><div class="tiprow"><span class="sw sw-ai"></span><span><b>AI</b> — written by an AI tool (Copilot, Cursor, Claude, Codex, Gemini, Devin)</span></div><div class="tiprow"><span class="sw sw-human"></span><span><b>Human</b> — typed by you</span></div><div class="tiprow"><span class="sw sw-paste"></span><span><b>Copy&amp;Paste</b> — pasted from the clipboard; counts as human, tracked as its own bucket</span></div></span></span></div><span class="r mono">usage</span></div>
      <div class="tlist">
        {{range .Tools}}
        <div class="trow t-{{.Name}}">
          <span class="tav">{{.Icon}}</span>
          <div class="tbody">
            <div class="ttop">
              <span class="tnm">{{.Name}}</span>
              {{if .Model}}<span class="tmodel mono">{{.Model}}</span>{{end}}
              <span class="tspc"></span>
              <span class="tln mono">{{.Lines}}<span class="tk"> lines</span>{{if .Deleted}} <span class="tdel">&minus;{{.Deleted}}</span>{{end}}</span>
            </div>
            <div class="tbar"><span class="tfl" style="width:{{.WidthPct}}%"></span></div>
            <div class="tmeta mono">
              {{if .HasAccept}}<span class="tpill">kept {{.Kept}}/{{.Suggested}} · {{.AcceptPct}}%</span>{{end}}
              {{if .HasTokens}}<span class="ttok"><span class="ar">↑</span>{{.TokIn}} <span class="ar">↓</span>{{.TokOut}} <span class="tdim">cache {{.TokCacheR}}/{{.TokCacheW}}</span></span>{{end}}
            </div>
          </div>
        </div>
        {{end}}
      </div>
    </div>
    {{end}}

    <div class="row2">
      <div class="card">
        <div class="ct"><div class="l"><span class="dot"></span><span class="t">Files</span></div><span class="r mono">{{.FilesChanged}} changed</span></div>
        {{range .Files}}
        <div class="frow">
          <span class="ic"><svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg></span>
          <span class="nm mono">{{.Name}}</span>
          {{if .Attr}}<span class="fattr mono"><span class="sw"></span>{{.Attr}} {{.AttrLines}}</span>{{else}}<span class="fdash mono">—</span>{{end}}
          <span class="fadd mono">+{{.Added}}</span>
          <span class="fdel mono{{if .Deleted}} on{{end}}">&minus;{{.Deleted}}</span>
          <span class="fbar"><span class="s-add" style="width:{{.AddedW}}%"></span><span class="s-del" style="width:{{.DeletedW}}%"></span></span>
        </div>
        {{else}}<div class="fdash mono" style="padding:11px 4px">no files</div>{{end}}
      </div>

      <div class="card">
        <div class="ct"><div class="l"><span class="dot"></span><span class="t">Leaderboard</span></div><span class="r mono">by lines</span></div>
        <div class="lb-l">
          {{range .Leaders}}
          <div class="lr">
            <span class="rk mono">{{.Rank}}</span>
            <span class="av {{if .IsModel}}l-model{{else}}l-human{{end}}">{{if .Icon}}{{.Icon}}{{else if .IsModel}}<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 1.6l1.7 6.3a2 2 0 0 0 1.4 1.4L21.4 11l-6.3 1.7a2 2 0 0 0-1.4 1.4L12 20.4l-1.7-6.3a2 2 0 0 0-1.4-1.4L2.6 11l6.3-1.7a2 2 0 0 0 1.4-1.4z"/></svg>{{else}}{{.Initial}}{{end}}</span>
            <span class="col">
              <span class="top"><span class="nm mono">{{.Name}}</span><span><span class="ln mono">{{.Lines}}</span> <span class="pc mono">{{.Pct}}%</span></span></span>
              <span class="tr"><span class="fl {{if .IsModel}}l-model{{else}}l-human{{end}}" style="width:{{.WidthPct}}%"></span></span>
            </span>
          </div>
          {{else}}<div class="fdash mono">no contributors</div>{{end}}
          <div class="lfoot">{{.Contributors}} contributor(s) · {{.Added}} lines added</div>
        </div>
      </div>
    </div>

    {{if .Files}}
    <div class="card ranges">
      <div class="ct"><div class="l"><span class="dot"></span><span class="t">File ranges</span></div></div>
      {{range .Files}}
      <div class="rfile">
        <span class="ic"><svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg></span>
        <span class="rname mono">{{.Name}}</span>
        <span class="fadd mono">+{{.Added}}</span>
        <span class="fdel mono{{if .Deleted}} on{{end}}">&minus;{{.Deleted}}</span>
      </div>
      {{range .Ranges}}
      <div class="rrow">
        <span class="rloc mono">{{.Loc}}</span>
        <span class="rkind mono">{{.Type}}</span>
        <span class="rattr mono {{if .IsAI}}ai{{else}}hum{{end}}">{{.Attr}}</span>
      </div>
      {{end}}
      {{end}}
    </div>
    {{end}}
  </div>

  <div class="foot">
    <span class="ic"><svg viewBox="0 0 24 24"><circle cx="9" cy="9" r="6"/><path d="M16.5 9.5a6 6 0 1 1-5 9.9"/></svg></span>
    <span class="h">tokens</span>
    <span class="kv mono"><span class="k">in</span><span class="v">{{.TokIn}}</span></span>
    <span class="kv mono"><span class="k">out</span><span class="v">{{.TokOut}}</span></span>
    <span class="kv mono"><span class="k">cache_read</span><span class="v">{{.TokCacheR}}</span></span>
    <span class="kv mono"><span class="k">cache_write</span><span class="v">{{.TokCacheW}}</span></span>
    <span class="vsep"></span>
    <span class="ic"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg></span>
    <span class="h">coding</span><span class="mono">{{.Coding}}</span>
    <span class="flow">first edit → commit</span>
  </div>
  <div class="ver mono">blamely {{.Version}}</div>
</div>
</body>
</html>`
