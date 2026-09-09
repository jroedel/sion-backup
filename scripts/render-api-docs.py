#!/usr/bin/env python3
"""Render docs/openapi.yaml into a readable single-page reference.

Generated rather than hand-written, so it cannot drift from the spec. Every
heading, field table and status code below is read out of the YAML; the only
thing this script contributes is the design.

    make api-docs      # writes docs/api.html

The output is an HTML fragment, not a whole document: it opens fine in a
browser on its own and can also be published as-is.
"""

import html
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
SPEC = ROOT / "docs" / "openapi.yaml"
OUT = ROOT / "docs" / "api.html"

METHOD_ORDER = ["get", "post", "put", "patch", "delete"]


def esc(s) -> str:
    return html.escape(str(s), quote=True)


def md(text: str) -> str:
    """The little of Markdown that appears in this spec's descriptions."""
    if not text:
        return ""

    out, para, bullets = [], [], []

    def flush_para():
        if para:
            out.append(f"<p>{inline(' '.join(para))}</p>")
            para.clear()

    def flush_bullets():
        if bullets:
            items = "".join(f"<li>{inline(b)}</li>" for b in bullets)
            out.append(f"<ul>{items}</ul>")
            bullets.clear()

    for raw in text.strip().split("\n"):
        line = raw.strip()

        if not line:
            flush_para()
            flush_bullets()
        elif line.startswith(("- ", "* ")):
            flush_para()
            bullets.append(line[2:])
        elif bullets:
            bullets[-1] += " " + line
        else:
            para.append(line)

    flush_para()
    flush_bullets()

    return "".join(out)


def inline(s: str) -> str:
    s = esc(s)
    s = re.sub(r"`([^`]+)`", r"<code>\1</code>", s)
    s = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", s)

    return s


def ref_name(node) -> str | None:
    ref = node.get("$ref") if isinstance(node, dict) else None

    return ref.rsplit("/", 1)[-1] if ref else None


def type_of(node) -> str:
    """A short, honest type label, with a link when it names a schema."""
    if not isinstance(node, dict):
        return "—"

    if name := ref_name(node):
        return f'<a class="tref" href="#schema-{esc(name)}">{esc(name)}</a>'

    for key in ("allOf", "oneOf", "anyOf"):
        if key in node:
            parts = [type_of(x) for x in node[key] if ref_name(x) or x.get("type")]
            parts = [p for p in parts if p not in ("—", "null")]

            return " or ".join(dict.fromkeys(parts)) or "—"

    t = node.get("type", "—")

    if t == "null":
        return "null"

    if fmt := node.get("format"):
        return f'{esc(t)} <span class="fmt">{esc(fmt)}</span>'

    if t == "integer" and "minimum" in node:
        return f'{esc(t)} <span class="fmt">≥ {esc(node["minimum"])}</span>'

    return esc(t)


def field_rows(schema: dict) -> str:
    props = schema.get("properties") or {}
    required = set(schema.get("required") or [])

    rows = []

    for name, node in props.items():
        node = node or {}
        req = '<span class="req">required</span>' if name in required else ""
        desc = md(node.get("description", ""))

        if ex := node.get("examples"):
            if not (isinstance(node.get("type"), str) and node.get("type") == "object"):
                shown = ", ".join(f"<code>{esc(e)}</code>" for e in ex[:6])
                desc += f'<p class="eg">e.g. {shown}</p>'

        rows.append(
            f'<tr><td class="fname"><code>{esc(name)}</code>{req}</td>'
            f'<td class="ftype">{type_of(node)}</td>'
            f'<td class="fdesc">{desc}</td></tr>'
        )

    if not rows:
        return ""

    return (
        '<div class="tablewrap"><table class="fields">'
        "<thead><tr><th>Field</th><th>Type</th><th>Notes</th></tr></thead>"
        f"<tbody>{''.join(rows)}</tbody></table></div>"
    )


def example_block(schema: dict) -> str:
    ex = schema.get("examples")

    if not ex or not isinstance(ex[0], dict):
        return ""

    body = yaml.safe_dump(ex[0], sort_keys=False, allow_unicode=True, width=76)

    return f'<pre class="code"><code>{esc(body.rstrip())}</code></pre>'


def body_schema(op: dict, spec: dict) -> tuple[str | None, dict | None]:
    rb = op.get("requestBody") or {}
    node = (rb.get("content") or {}).get("application/json", {}).get("schema")
    name = ref_name(node) if node else None

    return name, (spec["components"]["schemas"].get(name) if name else None)


def response_rows(op: dict, spec: dict) -> str:
    rows = []

    for code, node in (op.get("responses") or {}).items():
        node = node or {}

        if name := ref_name(node):
            node = spec["components"]["responses"][name]

        schema = (node.get("content") or {}).get("application/json", {}).get("schema")
        sname = ref_name(schema) if schema else None
        body = (
            f'<a class="tref" href="#schema-{esc(sname)}">{esc(sname)}</a>'
            if sname else '<span class="nobody">—</span>'
        )

        cls = "s2" if code.startswith(("2", "3")) else "s4" if code.startswith("4") else "s5"

        rows.append(
            f'<tr><td><span class="status {cls}">{esc(code)}</span></td>'
            f"<td>{md(node.get('description',''))}</td><td>{body}</td></tr>"
        )

    return (
        '<div class="tablewrap"><table class="fields responses">'
        "<thead><tr><th>Status</th><th>Meaning</th><th>Body</th></tr></thead>"
        f"<tbody>{''.join(rows)}</tbody></table></div>"
    )


def build(spec: dict) -> str:
    info = spec["info"]
    schemas = spec["components"]["schemas"]

    endpoints = []
    for path, item in spec["paths"].items():
        for method in METHOD_ORDER:
            if method in item:
                endpoints.append((method, path, item[method]))

    # ---- rail -------------------------------------------------------------
    rail = "".join(
        f'<a class="railitem" href="#op-{esc(op["operationId"])}">'
        f'<span class="m m-{m}">{m.upper()}</span>'
        f'<span class="rp">{esc(p)}</span></a>'
        for m, p, op in endpoints
    )

    rail += '<div class="railhead">Schemas</div>'
    rail += "".join(
        f'<a class="railitem sch" href="#schema-{esc(n)}"><span class="rp">{esc(n)}</span></a>'
        for n in schemas
    )

    # ---- endpoints --------------------------------------------------------
    ops = []
    for method, path, op in endpoints:
        oid = op["operationId"]
        secured = op.get("security") != []

        bname, bschema = body_schema(op, spec)
        req = ""

        if bschema:
            req = (
                f'<h4>Request body <a class="tref" href="#schema-{esc(bname)}">{esc(bname)}</a></h4>'
                + field_rows(bschema) + example_block(bschema)
            )

        ops.append(f"""
<section class="op" id="op-{esc(oid)}">
  <div class="opbar">
    <span class="m m-{method}">{method.upper()}</span>
    <code class="path">{esc(path)}</code>
    {'<span class="auth">machine token</span>' if secured
     else '<span class="auth open">no auth</span>'}
  </div>
  <h3>{esc(op.get('summary',''))}</h3>
  <div class="prose">{md(op.get('description',''))}</div>
  {req}
  <h4>Responses</h4>
  {response_rows(op, spec)}
</section>""")

    # ---- schemas ----------------------------------------------------------
    schema_blocks = []
    for name, schema in schemas.items():
        rows = field_rows(schema)

        if not rows:
            rows = f'<div class="prose scalar">{md(schema.get("description",""))}</div>'
            desc = ""
        else:
            desc = f'<div class="prose">{md(schema.get("description",""))}</div>'

        schema_blocks.append(f"""
<section class="schema" id="schema-{esc(name)}">
  <h3><code>{esc(name)}</code></h3>
  {desc}
  {rows}
  {example_block(schema)}
</section>""")

    counts = (
        f'<span><b>{len(endpoints)}</b> endpoints</span>'
        f'<span><b>{len(schemas)}</b> schemas</span>'
        f'<span><b>OpenAPI {esc(spec["openapi"])}</b></span>'
        f'<span><b>v{esc(info["version"])}</b></span>'
    )

    server = spec["servers"][0]["url"]

    return TEMPLATE.format(
        title=esc(info["title"]),
        summary=esc(info.get("summary", "")),
        intro=md(info.get("description", "")),
        counts=counts,
        server=esc(server),
        rail=rail,
        ops="".join(ops),
        schemas="".join(schema_blocks),
    )


TEMPLATE = """<title>{title}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Archivo:wght@500;600;700&family=Source+Serif+4:opsz,wght@8..60,400;8..60,600&family=IBM+Plex+Mono:wght@400;500;600&display=swap">
<style>
/* Generated by scripts/render-api-docs.py — edit the script, not this file. */

:root {{
  --ground:  #f2f3f6;
  --surface: #ffffff;
  --sunken:  #eceef2;
  --ink:     #191c24;
  --ink-2:   #565d70;
  --ink-3:   #7b8296;
  --rule:    #dcdfe7;
  --rule-2:  #c6cad6;
  --accent:  #454ea0;
  --accent-soft: #e8e9f6;
  --ok:   #1c6e46;
  --warn: #8a5c07;
  --bad:  #9d2f2f;
  --ok-soft:   #e2efe8;
  --warn-soft: #f7eeda;
  --bad-soft:  #f6e5e5;
  --shadow: 0 1px 2px rgba(25,28,36,.06), 0 8px 24px -16px rgba(25,28,36,.25);
}}

@media (prefers-color-scheme: dark) {{
  :root:not([data-theme="light"]) {{
    --ground:  #14161d;
    --surface: #1b1e27;
    --sunken:  #101218;
    --ink:     #e7e9f0;
    --ink-2:   #a4abbd;
    --ink-3:   #7d8497;
    --rule:    #2a2f3c;
    --rule-2:  #3a4152;
    --accent:  #9aa2ee;
    --accent-soft: #23263c;
    --ok:   #6cc396;
    --warn: #d7a44f;
    --bad:  #ea8a8a;
    --ok-soft:   #16281f;
    --warn-soft: #2a2213;
    --bad-soft:  #2b1a1a;
    --shadow: 0 1px 2px rgba(0,0,0,.4), 0 8px 24px -16px rgba(0,0,0,.7);
  }}
}}

:root[data-theme="dark"] {{
  --ground:  #14161d;
  --surface: #1b1e27;
  --sunken:  #101218;
  --ink:     #e7e9f0;
  --ink-2:   #a4abbd;
  --ink-3:   #7d8497;
  --rule:    #2a2f3c;
  --rule-2:  #3a4152;
  --accent:  #9aa2ee;
  --accent-soft: #23263c;
  --ok:   #6cc396;
  --warn: #d7a44f;
  --bad:  #ea8a8a;
  --ok-soft:   #16281f;
  --warn-soft: #2a2213;
  --bad-soft:  #2b1a1a;
  --shadow: 0 1px 2px rgba(0,0,0,.4), 0 8px 24px -16px rgba(0,0,0,.7);
}}

* {{ box-sizing: border-box; }}

body {{
  margin: 0;
  background: var(--ground);
  color: var(--ink);
  font-family: "Source Serif 4", Georgia, "Times New Roman", serif;
  font-size: 16.5px;
  line-height: 1.6;
  -webkit-font-smoothing: antialiased;
}}

code, pre, .m, .status, .fmt {{
  font-family: "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
}}

h1, h2, h3, h4, .label, .railhead, thead th, .auth {{
  font-family: Archivo, "Helvetica Neue", Arial, sans-serif;
}}

/* ---------- masthead ---------- */

.mast {{
  border-bottom: 1px solid var(--rule);
  background: var(--surface);
}}
.mastinner {{
  max-width: 78rem;
  margin: 0 auto;
  padding: 2.6rem 2rem 2rem;
}}
.eyebrow {{
  font-family: Archivo, sans-serif;
  font-size: .72rem;
  font-weight: 600;
  letter-spacing: .13em;
  text-transform: uppercase;
  color: var(--accent);
  margin: 0 0 .7rem;
}}
h1 {{
  font-size: clamp(1.9rem, 4vw, 2.7rem);
  font-weight: 700;
  letter-spacing: -.018em;
  line-height: 1.1;
  margin: 0 0 .5rem;
  text-wrap: balance;
}}
.summary {{
  font-size: 1.12rem;
  color: var(--ink-2);
  max-width: 46rem;
  margin: 0 0 1.4rem;
}}
.counts {{
  display: flex;
  flex-wrap: wrap;
  gap: 1.4rem;
  font-family: Archivo, sans-serif;
  font-size: .8rem;
  color: var(--ink-3);
  border-top: 1px solid var(--rule);
  padding-top: 1rem;
}}
.counts b {{
  color: var(--ink);
  font-variant-numeric: tabular-nums;
  font-weight: 600;
}}
.server {{
  margin-top: 1.1rem;
  font-size: .84rem;
  color: var(--ink-3);
}}
.server code {{ color: var(--ink-2); }}

/* ---------- shell ---------- */

.shell {{
  max-width: 78rem;
  margin: 0 auto;
  padding: 2.2rem 2rem 5rem;
  display: grid;
  grid-template-columns: 15rem minmax(0, 1fr);
  gap: 3rem;
  align-items: start;
}}
@media (max-width: 60rem) {{
  .shell {{ grid-template-columns: minmax(0, 1fr); gap: 1.5rem; }}
  nav.rail {{ position: static; max-height: none; }}
}}

nav.rail {{
  position: sticky;
  top: 1.5rem;
  max-height: calc(100vh - 3rem);
  overflow-y: auto;
  font-family: Archivo, sans-serif;
  border-left: 2px solid var(--rule);
  padding-left: .9rem;
}}
.railhead {{
  font-size: .68rem;
  font-weight: 700;
  letter-spacing: .13em;
  text-transform: uppercase;
  color: var(--ink-3);
  margin: 1.5rem 0 .5rem;
}}
.railitem {{
  display: flex;
  align-items: baseline;
  gap: .5rem;
  padding: .28rem 0;
  text-decoration: none;
  color: var(--ink-2);
  font-size: .8rem;
  line-height: 1.35;
}}
.railitem:hover {{ color: var(--accent); }}
.railitem .rp {{ word-break: break-all; }}
.railitem.sch .rp {{ font-family: "IBM Plex Mono", monospace; font-size: .76rem; }}

/* ---------- method chips ---------- */

.m {{
  font-size: .64rem;
  font-weight: 600;
  letter-spacing: .06em;
  padding: .18rem .4rem;
  border-radius: 3px;
  flex: none;
  border: 1px solid transparent;
}}
.m-get  {{ color: var(--accent); background: var(--accent-soft); border-color: color-mix(in srgb, var(--accent) 25%, transparent); }}
.m-post {{ color: var(--ok); background: var(--ok-soft); border-color: color-mix(in srgb, var(--ok) 25%, transparent); }}

/* ---------- prose ---------- */

.prose p {{ margin: 0 0 .85rem; max-width: 62ch; }}
.prose ul {{ margin: 0 0 .9rem; padding-left: 1.1rem; max-width: 62ch; }}
.prose li {{ margin-bottom: .35rem; }}
.prose strong {{ font-weight: 600; }}

.intro {{
  max-width: 62ch;
  border-left: 3px solid var(--accent);
  padding-left: 1.1rem;
  margin-bottom: 2.6rem;
}}
.intro p:last-child {{ margin-bottom: 0; }}

code {{
  font-size: .86em;
  background: var(--sunken);
  border: 1px solid var(--rule);
  border-radius: 3px;
  padding: .06em .3em;
}}
a.tref {{
  font-family: "IBM Plex Mono", monospace;
  font-size: .82rem;
  color: var(--accent);
  text-decoration: none;
  border-bottom: 1px dotted currentColor;
}}
a.tref:hover {{ border-bottom-style: solid; }}

/* ---------- sections ---------- */

h2.sect {{
  font-size: .74rem;
  font-weight: 700;
  letter-spacing: .14em;
  text-transform: uppercase;
  color: var(--ink-3);
  margin: 3.4rem 0 1.4rem;
  padding-bottom: .55rem;
  border-bottom: 1px solid var(--rule-2);
}}
h2.sect:first-of-type {{ margin-top: 0; }}

.op, .schema {{
  background: var(--surface);
  border: 1px solid var(--rule);
  border-radius: 6px;
  padding: 1.5rem 1.6rem;
  margin-bottom: 1.5rem;
  box-shadow: var(--shadow);
  scroll-margin-top: 1.5rem;
}}
.opbar {{
  display: flex;
  align-items: center;
  gap: .7rem;
  flex-wrap: wrap;
  padding-bottom: .9rem;
  border-bottom: 1px solid var(--rule);
  margin-bottom: 1rem;
}}
.path {{
  font-size: .95rem;
  font-weight: 500;
  background: none;
  border: none;
  padding: 0;
  color: var(--ink);
  word-break: break-all;
}}
.auth {{
  margin-left: auto;
  font-size: .68rem;
  font-weight: 600;
  letter-spacing: .07em;
  text-transform: uppercase;
  color: var(--ink-3);
  border: 1px solid var(--rule-2);
  border-radius: 999px;
  padding: .12rem .55rem;
}}
.auth.open {{ color: var(--warn); border-color: color-mix(in srgb, var(--warn) 45%, transparent); background: var(--warn-soft); }}

.op h3, .schema h3 {{
  font-size: 1.16rem;
  font-weight: 600;
  letter-spacing: -.01em;
  margin: 0 0 .7rem;
  text-wrap: balance;
}}
.schema h3 code {{ background: none; border: none; padding: 0; font-size: 1rem; }}
h4 {{
  font-size: .7rem;
  font-weight: 700;
  letter-spacing: .13em;
  text-transform: uppercase;
  color: var(--ink-3);
  margin: 1.6rem 0 .6rem;
  display: flex;
  align-items: baseline;
  gap: .6rem;
}}

/* ---------- tables ---------- */

.tablewrap {{ overflow-x: auto; }}
table.fields {{
  width: 100%;
  border-collapse: collapse;
  font-size: .88rem;
}}
table.fields th {{
  text-align: left;
  font-size: .66rem;
  font-weight: 700;
  letter-spacing: .11em;
  text-transform: uppercase;
  color: var(--ink-3);
  padding: 0 .8rem .45rem 0;
  border-bottom: 1px solid var(--rule-2);
  white-space: nowrap;
}}
table.fields td {{
  padding: .7rem .8rem .7rem 0;
  border-bottom: 1px solid var(--rule);
  vertical-align: top;
}}
table.fields tr:last-child td {{ border-bottom: none; }}
.fname {{ white-space: nowrap; }}
.fname code {{ background: none; border: none; padding: 0; font-weight: 500; color: var(--ink); }}
.ftype {{ font-family: "IBM Plex Mono", monospace; font-size: .78rem; color: var(--ink-2); white-space: nowrap; }}
.fmt {{ color: var(--ink-3); font-size: .95em; }}
.fdesc {{ min-width: 20rem; }}
.fdesc p {{ margin: 0 0 .5rem; }}
.fdesc p:last-child {{ margin-bottom: 0; }}
.fdesc ul {{ margin: .3rem 0 .5rem; padding-left: 1.1rem; }}
.req {{
  display: block;
  font-family: Archivo, sans-serif;
  font-size: .6rem;
  font-weight: 600;
  letter-spacing: .09em;
  text-transform: uppercase;
  color: var(--bad);
  margin-top: .15rem;
}}
.eg {{ color: var(--ink-3); font-size: .93em; }}
.eg code {{ font-size: .82em; }}

.status {{
  font-size: .78rem;
  font-weight: 600;
  padding: .1rem .42rem;
  border-radius: 3px;
  font-variant-numeric: tabular-nums;
}}
.s2 {{ color: var(--ok);   background: var(--ok-soft); }}
.s4 {{ color: var(--warn); background: var(--warn-soft); }}
.s5 {{ color: var(--bad);  background: var(--bad-soft); }}
.responses td:first-child {{ white-space: nowrap; }}
.nobody {{ color: var(--ink-3); }}

pre.code {{
  background: var(--sunken);
  border: 1px solid var(--rule);
  border-radius: 5px;
  padding: .9rem 1rem;
  overflow-x: auto;
  font-size: .8rem;
  line-height: 1.55;
  margin: .7rem 0 0;
}}
pre.code code {{ background: none; border: none; padding: 0; font-size: 1em; }}

.scalar {{ color: var(--ink-2); font-size: .92rem; }}

footer {{
  max-width: 78rem;
  margin: 0 auto;
  padding: 2rem 2rem 4rem;
  border-top: 1px solid var(--rule);
  color: var(--ink-3);
  font-size: .86rem;
}}
footer p {{ max-width: 62ch; }}

a {{ color: var(--accent); }}
:focus-visible {{ outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 2px; }}
@media (prefers-reduced-motion: reduce) {{ * {{ animation: none !important; transition: none !important; }} }}
</style>

<header class="mast">
  <div class="mastinner">
    <p class="eyebrow">sion-backup · fleet contract</p>
    <h1>{title}</h1>
    <p class="summary">{summary}</p>
    <div class="counts">{counts}</div>
    <p class="server">Base URL <code>{server}</code></p>
  </div>
</header>

<div class="shell">
  <nav class="rail" aria-label="Contents">{rail}</nav>
  <main>
    <div class="intro prose">{intro}</div>
    <h2 class="sect">Endpoints</h2>
    {ops}
    <h2 class="sect">Schemas</h2>
    {schemas}
  </main>
</div>

<footer>
  <p>Generated from <code>docs/openapi.yaml</code> by <code>scripts/render-api-docs.py</code>.
  The YAML is the machine-readable contract and <code>docs/eumaeus-api.md</code> is the
  authority on behaviour — idempotency, audit rules, and what the client does with each
  status. Where this page and the markdown disagree, the markdown is right.</p>
</footer>
"""


def main() -> int:
    spec = yaml.safe_load(SPEC.read_text())
    OUT.write_text(build(spec))
    print(f"ok    wrote {OUT.relative_to(ROOT)} ({OUT.stat().st_size:,} bytes)")

    return 0


if __name__ == "__main__":
    sys.exit(main())
