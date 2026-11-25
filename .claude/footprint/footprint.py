#!/usr/bin/env python3
"""Estimate the energy and water used by the Claude Code sessions that worked on this repo,
and write it into README.md between the ai-footprint markers.

Run by a Claude Code Stop hook (.claude/settings.json) after every reply; safe to run by hand:
    python3 .claude/footprint/footprint.py          # update ledger + README
    python3 .claude/footprint/footprint.py --print  # also print the totals

How it works
- Reads every transcript in ~/.claude/projects/**/*.jsonl. Each API call's real token usage is
  recorded there (input, cache write, cache read, output; thinking counts as output).
- A call belongs to this repo when its working directory is inside the repo, or when it was made
  from the folder above (the workspace) and its tool calls name this repo. Calls that name no repo
  belong to the repo(s) the session last worked on. A call naming two repos is split evenly.
  The sibling repos (other git folders next to this one) are worked out the same way, so each
  repo only counts its own share.
- Per-session totals are kept in ledger.json (commit it). Claude Code deletes old transcripts
  (30 days by default); the ledger keeps their numbers, so the totals only ever grow.

The estimate (central figures; reality could be ~3x lower or higher; Anthropic publishes no
per-token energy numbers). Energy is per 1,000 tokens, at the servers:
- output tokens 1.5 Wh, uncached input and cache writes 0.1 Wh, cache reads 0.015 Wh
- data-centre overhead (PUE) 1.15
- water: on-site cooling 0.5 L per server kWh, plus 3.1 L per kWh for generating the electricity
- reference: a 1.5-ton AC draws about 1.5 kW
"""
import json
import os
import re
import sys
import tempfile
from datetime import datetime
from pathlib import Path

WH_PER_1K = {"out": 1.5, "in": 0.1, "cw": 0.1, "cr": 0.015}
PUE = 1.15
WUE_ONSITE_L_PER_KWH = 0.5
WATER_GRID_L_PER_KWH = 3.1
AC_KW = 1.5
BOTTLE_L = 0.5

START = "<!-- ai-footprint:start -->"
END = "<!-- ai-footprint:end -->"
TOKEN_KEYS = ("in", "cw", "cr", "out")
USAGE_KEYS = {"in": "input_tokens", "cw": "cache_creation_input_tokens",
              "cr": "cache_read_input_tokens", "out": "output_tokens"}

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
WORKSPACE = REPO.parent
LEDGER = HERE / "ledger.json"
README = REPO / "README.md"
PROJECTS = Path(os.environ.get("CLAUDE_CONFIG_DIR", Path.home() / ".claude")) / "projects"
CACHE = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "claude-footprint"


def repos():
    """This repo and its sibling git repos: name -> absolute path."""
    out = {REPO.name: REPO}
    for d in WORKSPACE.iterdir():
        if d.is_dir() and (d / ".git").exists():
            out[d.name] = d
    return out


def ref_pattern(name):
    # The name as a whole path part: "better-red" must not match "better-red-broker".
    edge_l = r"(?:^|(?<=[\s/'\"`(=:]))"
    edge_r = r"(?=$|[\s/'\"`),;:\]])"
    return re.compile(edge_l + re.escape(name) + edge_r, re.M)


def strings(o):
    if isinstance(o, str):
        yield o
    elif isinstance(o, dict):
        for v in o.values():
            yield from strings(v)
    elif isinstance(o, list):
        for v in o:
            yield from strings(v)


def inside(path, root):
    root = str(root)
    return path == root or path.startswith(root + os.sep)


def session_share(path, all_repos, patterns):
    """This repo's share of one transcript, or None if it has none."""
    calls = {}  # message id -> call, in first-seen order
    for line in path.open(encoding="utf-8", errors="replace"):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        msg = e.get("message") if isinstance(e, dict) else None
        if e.get("type") != "assistant" or not isinstance(msg, dict) or not msg.get("usage"):
            continue
        if msg.get("model") == "<synthetic>":
            continue
        mid = msg.get("id") or e.get("uuid")
        c = calls.setdefault(mid, {"refs": set(), "cwd": e.get("cwd") or "", "ts": e.get("timestamp", "")})
        c["usage"] = msg["usage"]  # the last entry of a message has the final counts
        c["model"] = msg.get("model", "?")
        c["effort"] = e.get("effort") or c.get("effort")
        for block in msg.get("content") or []:
            if isinstance(block, dict) and block.get("type") == "tool_use":
                text = "\n".join(strings(block.get("input")))
                c["refs"].update(n for n, p in patterns.items() if p.search(text))

    share = {k: 0.0 for k in TOKEN_KEYS}
    share.update(calls=0.0, models={}, first=None, last=None)
    last, pending = None, []

    def add(c, refs):
        if REPO.name not in refs:
            return
        w = 1 / len(refs)
        for k in TOKEN_KEYS:
            share[k] += (c["usage"].get(USAGE_KEYS[k]) or 0) * w
        share["calls"] += w
        label = c["model"] + (f" ({c['effort']})" if c.get("effort") else "")
        share["models"][label] = share["models"].get(label, 0) + w
        try:  # local date
            day = datetime.fromisoformat(c["ts"].replace("Z", "+00:00")).astimezone().date().isoformat()
        except ValueError:
            day = None
        if day:
            share["first"] = min(filter(None, [share["first"], day]))
            share["last"] = max(filter(None, [share["last"], day]))

    for c in calls.values():
        here = [n for n, p in all_repos.items() if inside(c["cwd"], p)]
        if here:
            refs = set(here)
        elif inside(c["cwd"], WORKSPACE):
            refs = c["refs"] or last
        else:
            continue  # another project; ignore
        if not refs:
            pending.append(c)  # before the session named any repo: give it the first one named
            continue
        for p in pending:
            add(p, refs)
        pending = []
        add(c, refs)
        last = refs
    return share if share["calls"] else None


def load(path, default):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return default


def save(path, data, text=None):
    path.parent.mkdir(parents=True, exist_ok=True)
    body = text if text is not None else json.dumps(data, indent=1, sort_keys=True) + "\n"
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=path.name + ".")
    with os.fdopen(fd, "w") as f:
        f.write(body)
    os.replace(tmp, path)


def update_ledger():
    ledger = load(LEDGER, {"sessions": {}})
    cache_file = CACHE / (re.sub(r"[^\w.-]", "_", str(REPO)) + ".json")
    seen = load(cache_file, {})
    all_repos = repos()
    patterns = {n: ref_pattern(n) for n in all_repos}
    needle = REPO.name.encode()
    changed = False
    for path in sorted(PROJECTS.rglob("*.jsonl")) if PROJECTS.is_dir() else []:
        st = path.stat()
        sig = [st.st_size, st.st_mtime_ns]
        if seen.get(str(path)) == sig:
            continue
        seen[str(path)] = sig
        rel = path.relative_to(PROJECTS).with_suffix("")
        key = "/".join(rel.parts[1:])  # session id (plus subagents/<agent> for subagents)
        with path.open("rb") as f:
            hit = needle in f.read()
        share = session_share(path, all_repos, patterns) if hit else None
        if share:
            for k in TOKEN_KEYS:
                share[k] = round(share[k])
            share["calls"] = round(share["calls"], 2)
            share["models"] = {m: round(v, 2) for m, v in share["models"].items()}
            if ledger["sessions"].get(key) != share:
                ledger["sessions"][key] = share
                changed = True
    if changed:
        save(LEDGER, ledger)
    save(cache_file, seen)
    return ledger


def estimate(t):
    it_kwh = sum(t[k] / 1000 * WH_PER_1K[k] for k in TOKEN_KEYS) / 1000
    kwh = it_kwh * PUE
    cooling = it_kwh * WUE_ONSITE_L_PER_KWH
    water = cooling + kwh * WATER_GRID_L_PER_KWH
    return kwh, water, cooling


def num(x):
    return f"{x:.2f}" if x < 1 else f"{x:.1f}" if x < 10 else f"{x:,.0f}"


def model_name(label):
    # "claude-opus-5-5 (high)" -> "Opus 5.5 (high)"
    m = re.match(r"claude-([a-z]+)-(\d+)-(\d+)(.*)", label)
    return f"{m[1].title()} {m[2]}.{m[3]}{m[4]}" if m else label


def render(ledger):
    s = ledger["sessions"].values()
    t = {k: sum(x[k] for x in s) for k in TOKEN_KEYS}
    calls = sum(x["calls"] for x in s)
    models = {}
    for x in s:
        for m, v in x["models"].items():
            models[m] = models.get(m, 0) + v
    kwh, water, cooling = estimate(t)
    tokens = sum(t.values())
    ac_h = kwh / AC_KW
    ac = f"~{ac_h * 60:.0f} minutes" if ac_h < 1 else f"~{num(ac_h)} hours"
    used = ", ".join(model_name(m) for m, _ in sorted(models.items(), key=lambda kv: -kv[1]))
    last = max((x["last"] for x in s if x.get("last")), default="")
    return "\n".join([
        START,
        "**🌍 AI footprint so far** · built with Claude Code",
        "",
        "| ⚡ Energy | 💧 Water | 🧠 Tokens processed | 💬 Sessions | 🔁 Model calls |",
        "|:-:|:-:|:-:|:-:|:-:|",
        f"| **~{num(kwh)} kWh** | **~{num(water)} L** | {tokens / 1e6:,.1f} M | {len(s)} | {calls:,.0f} |",
        "",
        f"For reference: that's about the electricity a 1.5-ton AC uses in {ac}, and"
        f" ~{water / BOTTLE_L:,.0f} half-litre bottles of water.",
        "",
        f"<sub>Model: {used or 'n/a'}. Estimated from Claude Code session logs (could be ~3× lower or"
        f" higher); water includes on-site cooling (~{num(cooling)} L) and generating the electricity."
        f" Updated automatically, last on {last or 'n/a'}."
        f" [How it's calculated](.claude/footprint/footprint.py).</sub>",
        END,
    ])


def write_readme(block):
    text = README.read_text(encoding="utf-8")
    if START in text and END in text:
        a = text.index(START)
        b = text.index(END) + len(END)
        new = text[:a] + block + text[b:]
    else:
        # First run: right under the "> ⚠️ Vibe coded" line.
        m = re.search(r"^>.*vibe coded.*$", text, re.I | re.M)
        at = m.end() if m else 0
        new = text[:at] + "\n\n" + block + text[at:] if m else block + "\n\n" + text
    if new != text:
        save(README, None, new)


def main():
    ledger = update_ledger()
    if ledger["sessions"]:
        write_readme(render(ledger))
    if "--print" in sys.argv:
        print(render(ledger))


if __name__ == "__main__":
    try:
        main()
    except Exception as err:  # never break a Claude Code session over this
        print(f"ai-footprint: {err}", file=sys.stderr)
    sys.exit(0)
