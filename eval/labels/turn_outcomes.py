#!/usr/bin/env python3
"""Freeze and score the owner-labeled P0 turn-outcome sample (#402)."""

import argparse
import hashlib
import json
import os
import shlex
import sqlite3
from collections import Counter
from contextlib import closing
from datetime import datetime
from pathlib import Path


# Fifty rows: preserve the accepted-precision target, include revised turns,
# and cover all four projects with settled rows. Revised exists only in vishnu.
QUOTAS = {
    "accepted": {"nexus": 10, "vishnu": 10},
    "revised": {"vishnu": 10},
    "failed": {"nexus": 3, "vishnu": 3, "valo-backend": 2, "kiosk": 2},
    "unknown": {"nexus": 3, "vishnu": 3, "valo-backend": 2, "kiosk": 2},
}
GOLD_LABELS = ("accepted", "revised", "rejected", "unclear")
PREDICTED_LABEL = {
    "accepted": "accepted",
    "revised": "revised",
    "failed": "rejected",
    "unknown": "unclear",
}
DEFAULT_SEED = "p0-turn-outcomes-2026-11-08"
DEFAULT_CUTOFF = "2026-11-08T00:00:00Z"
FROZEN_MANIFEST_SHA256 = (
    "59c9311cf6561174ee379b6f64ee1a3ab293d7c90ebe6ffeaf0de7af29f94f1a"
)


def default_registry():
    if directory := os.environ.get("CRUSH_GLOBAL_DATA"):
        return Path(directory) / "projects.json"
    if directory := os.environ.get("XDG_DATA_HOME"):
        return Path(directory) / "crush" / "projects.json"
    return Path.home() / ".local" / "share" / "crush" / "projects.json"


def cutoff_millis(value):
    try:
        instant = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise ValueError(f"invalid cutoff {value!r}") from error
    if instant.tzinfo is None:
        raise ValueError("cutoff must include a timezone")
    return int(instant.timestamp() * 1000)


def project_databases(registry):
    entries = json.loads(registry.read_text())["projects"]
    wanted = {project for by_project in QUOTAS.values() for project in by_project}
    databases = {}
    for entry in entries:
        name = Path(entry["path"]).name
        if name not in wanted:
            continue
        if name in databases:
            raise ValueError(f"multiple registered projects named {name!r}")
        databases[name] = Path(entry["data_dir"]) / "crush.db"
    missing = sorted(wanted - databases.keys())
    if missing:
        raise ValueError(f"projects missing from registry: {', '.join(missing)}")
    return databases


def read_candidates(databases, cutoff):
    candidates = []
    for project, db_path in sorted(databases.items()):
        if not db_path.is_file():
            raise ValueError(f"{project}: database missing at {db_path}")
        with closing(sqlite3.connect(db_path.as_uri() + "?mode=ro", uri=True)) as conn:
            project_keys = [
                row[0]
                for row in conn.execute("SELECT DISTINCT project_key FROM turn_outcomes")
            ]
            if len(project_keys) != 1:
                raise ValueError(
                    f"{project}: expected one project partition, found {len(project_keys)}"
                )
            rows = conn.execute(
                """SELECT t.session_id, t.turn_id, t.verdict
                   FROM turn_outcomes t
                   JOIN messages m ON m.id = t.turn_id AND m.session_id = t.session_id
                   WHERE t.project_key = ? AND t.settled_at IS NOT NULL
                     AND t.settled_at < ?
                   ORDER BY t.session_id, t.turn_id""",
                (project_keys[0], cutoff),
            )
            for session_id, turn_id, verdict in rows:
                if verdict not in QUOTAS:
                    raise ValueError(f"{project}: unexpected settled verdict {verdict!r}")
                candidates.append(
                    {
                        "project": project,
                        "session_id": session_id,
                        "turn_id": turn_id,
                        "settled_verdict": verdict,
                    }
                )
    return candidates


def digest(*parts):
    return hashlib.sha256("\0".join(parts).encode()).hexdigest()


def select_sample(candidates, seed=DEFAULT_SEED):
    chosen = []
    for verdict, by_project in QUOTAS.items():
        for project, quota in by_project.items():
            pool = [
                row
                for row in candidates
                if row["project"] == project and row["settled_verdict"] == verdict
            ]
            if len(pool) < quota:
                raise ValueError(
                    f"{project}/{verdict}: need {quota} settled reviewable turns, "
                    f"found {len(pool)}"
                )
            ranked = sorted(
                pool,
                key=lambda row: digest(
                    seed, project, row["session_id"], row["turn_id"]
                ),
            )
            chosen.extend(ranked[:quota])
    for row in chosen:
        row["sample_id"] = digest(
            row["project"], row["session_id"], row["turn_id"]
        )[:16]
    if len({row["sample_id"] for row in chosen}) != len(chosen):
        raise ValueError("sample ID collision or duplicate turn")
    return sorted(chosen, key=lambda row: digest(seed, "review", row["sample_id"]))


def write_jsonl(path, rows):
    with path.open("x") as output:
        for row in rows:
            output.write(json.dumps(row, sort_keys=True) + "\n")


def load_jsonl(path):
    with path.open() as source:
        return [json.loads(line) for line in source if line.strip()]


def message_text(parts_json):
    parts = json.loads(parts_json)
    return "\n".join(
        part.get("data", {}).get("text", "")
        for part in parts
        if part.get("type") == "text"
    ).strip()


def review_context(db_path, row):
    with closing(sqlite3.connect(db_path.as_uri() + "?mode=ro", uri=True)) as conn:
        messages = conn.execute(
            "SELECT id, role, parts FROM messages WHERE session_id = ? ORDER BY rowid",
            (row["session_id"],),
        ).fetchall()
        selected = next(
            (i for i, message in enumerate(messages) if message[0] == row["turn_id"]),
            None,
        )
        if selected is None or messages[selected][1] != "user":
            raise ValueError(f"{row['sample_id']}: user turn missing from session")
        edited = conn.execute(
            "SELECT edited_paths FROM turn_outcomes WHERE session_id = ? AND turn_id = ?",
            (row["session_id"], row["turn_id"]),
        ).fetchone()
        if edited is None:
            raise ValueError(f"{row['sample_id']}: outcome row missing")

    assistant_texts = []
    next_user = ""
    for _, role, parts in messages[selected + 1 :]:
        if role == "user":
            next_user = message_text(parts)
            break
        if role == "assistant":
            text = message_text(parts)
            if text:
                assistant_texts.append(text)
    edited_paths = json.loads(edited[0] or "[]")
    if not isinstance(edited_paths, list):
        edited_paths = []
    return {
        "prompt": message_text(messages[selected][2]),
        "answer": assistant_texts[-1] if assistant_texts else "",
        "next_user": next_user,
        "edited_paths": edited_paths,
    }


def print_review(index, total, row, db_path, context):
    def excerpt(value, limit=3000):
        if not value:
            return "(none recorded)"
        return value if len(value) <= limit else value[:limit] + "\n[truncated]"

    print(f"\n=== {index}/{total}: {row['sample_id']} ({row['project']}) ===")
    print(f"session_id: {row['session_id']}")
    print(f"turn_id: {row['turn_id']}")
    print(f"edited paths: {', '.join(context['edited_paths']) or '(none)'}")
    print(f"\nUSER PROMPT\n{excerpt(context['prompt'])}")
    print(f"\nLAST ASSISTANT TEXT BEFORE NEXT USER MESSAGE\n{excerpt(context['answer'])}")
    print(f"\nNEXT USER-ROLE MESSAGE (MAY BE A HARNESS FOLLOW-UP)\n{excerpt(context['next_user'], 1500)}")
    command = (
        f"crush --data-dir {shlex.quote(str(db_path.parent))} "
        f"session show {shlex.quote(row['session_id'])} --json"
    )
    print(f"\nFull transcript: {command}")
    print("Choose accepted, revised, rejected, or unclear; an empty note is fine.")


def verify_frozen_manifest(path):
    actual = hashlib.sha256(path.read_bytes()).hexdigest()
    if actual != FROZEN_MANIFEST_SHA256:
        raise ValueError(
            "manifest differs from the pre-registered sample "
            f"(expected {FROZEN_MANIFEST_SHA256}, got {actual})"
        )


def score(manifest, review):
    by_id = {row["sample_id"]: row for row in manifest}
    if len(by_id) != len(manifest):
        raise ValueError("duplicate sample IDs in manifest")
    if len(review) != len(manifest) or {row["sample_id"] for row in review} != set(by_id):
        raise ValueError("review sheet does not match frozen manifest")

    gold = []
    for row in review:
        source = by_id[row["sample_id"]]
        for field in ("project", "session_id", "turn_id"):
            if row.get(field) != source[field]:
                raise ValueError(f"{row['sample_id']}: {field} differs from manifest")
        label = row.get("gold_label", "")
        if label not in GOLD_LABELS:
            raise ValueError(f"{row['sample_id']}: missing or invalid gold label {label!r}")
        note = row.get("note", "").strip()
        gold.append({**source, "gold_label": label, "note": note})

    matrix = Counter((row["settled_verdict"], row["gold_label"]) for row in gold)
    return gold, matrix


def print_score(matrix):
    print("settled \\ gold   " + "  ".join(f"{label:>8}" for label in GOLD_LABELS))
    for verdict in QUOTAS:
        cells = [matrix[verdict, label] for label in GOLD_LABELS]
        total = sum(cells)
        correct = matrix[verdict, PREDICTED_LABEL[verdict]]
        print(
            f"{verdict:>14}  "
            + "  ".join(f"{cell:>8}" for cell in cells)
            + f"  precision={correct}/{total}"
        )
    for label in GOLD_LABELS:
        total = sum(matrix[verdict, label] for verdict in QUOTAS)
        matching = next(verdict for verdict, mapped in PREDICTED_LABEL.items() if mapped == label)
        print(f"recall {label}: {matrix[matching, label]}/{total}")
    accepted = sum(matrix["accepted", label] for label in GOLD_LABELS)
    precision = matrix["accepted", "accepted"] / accepted
    if precision >= 0.8:
        decision = "accepted mining may proceed"
    elif precision >= 0.6:
        decision = "mine only with an evaluator pass per episode"
    else:
        decision = "accepted mining remains blocked"
    print(f"accepted precision: {precision:.1%}; {decision}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    sample = commands.add_parser("sample", help="freeze a blind 50-turn review sheet")
    sample.add_argument("--registry", type=Path, default=default_registry())
    sample.add_argument("--cutoff", default=DEFAULT_CUTOFF)
    sample.add_argument("--seed", default=DEFAULT_SEED)
    sample.add_argument("--out-dir", type=Path, default=Path(__file__).parent)
    inspect = commands.add_parser("review", help="show evidence for blind owner labeling")
    inspect.add_argument("--registry", type=Path, default=default_registry())
    inspect.add_argument(
        "--review", type=Path, default=Path(__file__).parent / "turn_outcomes_review.draft.jsonl"
    )
    inspect.add_argument("--index", type=int, help="1-based row number; omit to show all")
    report = commands.add_parser("score", help="score completed owner labels")
    report.add_argument("--manifest", type=Path, required=True)
    report.add_argument("--review", type=Path, required=True)
    report.add_argument("--gold-out", type=Path)
    args = parser.parse_args()

    try:
        if args.command == "sample":
            candidates = read_candidates(
                project_databases(args.registry), cutoff_millis(args.cutoff)
            )
            selected = select_sample(candidates, args.seed)
            review = [
                {key: row[key] for key in ("sample_id", "project", "session_id", "turn_id")}
                | {"gold_label": "", "note": ""}
                for row in selected
            ]
            review_path = args.out_dir / "turn_outcomes_review.draft.jsonl"
            manifest_path = args.out_dir / "turn_outcomes_manifest.draft.jsonl"
            if review_path.exists() or manifest_path.exists():
                raise ValueError("draft already exists; keep the frozen sample")
            args.out_dir.mkdir(parents=True, exist_ok=True)
            write_jsonl(review_path, review)
            write_jsonl(manifest_path, selected)
            counts = Counter(row["settled_verdict"] for row in selected)
            print(f"frozen {len(selected)} turns before {args.cutoff}: {dict(counts)}")
            print(f"blind review: {review_path}")
            print(f"manifest: {manifest_path}")
            print(f"manifest SHA-256: {hashlib.sha256(manifest_path.read_bytes()).hexdigest()}")
        elif args.command == "review":
            rows = load_jsonl(args.review)
            if args.index is not None and not 1 <= args.index <= len(rows):
                raise ValueError(f"index must be between 1 and {len(rows)}")
            databases = project_databases(args.registry)
            indices = [args.index - 1] if args.index is not None else range(len(rows))
            for index in indices:
                row = rows[index]
                db_path = databases[row["project"]]
                print_review(index + 1, len(rows), row, db_path, review_context(db_path, row))
        else:
            verify_frozen_manifest(args.manifest)
            gold, matrix = score(load_jsonl(args.manifest), load_jsonl(args.review))
            print_score(matrix)
            if args.gold_out:
                write_jsonl(args.gold_out, gold)
                print(f"gold rows: {args.gold_out}")
    except (OSError, sqlite3.Error, ValueError, KeyError, json.JSONDecodeError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
