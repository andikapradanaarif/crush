import sqlite3
import tempfile
import unittest
from collections import Counter
from pathlib import Path

import turn_outcomes as labels


class TurnOutcomeLabelsTest(unittest.TestCase):
    def test_sample_is_balanced_and_deterministic(self):
        candidates = []
        for verdict, by_project in labels.QUOTAS.items():
            for project, quota in by_project.items():
                for n in range(quota + 5):
                    candidates.append(
                        {
                            "project": project,
                            "session_id": f"{project}-{verdict}-{n}",
                            "turn_id": f"turn-{n}",
                            "settled_verdict": verdict,
                        }
                    )

        first = labels.select_sample(candidates)
        second = labels.select_sample(list(reversed(candidates)))
        self.assertEqual(first, second)
        self.assertEqual(len(first), 50)
        self.assertEqual(
            Counter((row["settled_verdict"], row["project"]) for row in first),
            Counter(
                {
                    (verdict, project): quota
                    for verdict, by_project in labels.QUOTAS.items()
                    for project, quota in by_project.items()
                }
            ),
        )
        self.assertEqual(len({row["sample_id"] for row in first}), 50)

    def test_score_requires_complete_blind_labels(self):
        manifest = [
            {
                "sample_id": "a",
                "project": "nexus",
                "session_id": "session-a",
                "turn_id": "turn-a",
                "settled_verdict": "accepted",
            },
            {
                "sample_id": "b",
                "project": "vishnu",
                "session_id": "session-b",
                "turn_id": "turn-b",
                "settled_verdict": "revised",
            },
        ]
        review = [
            {
                "sample_id": "a",
                "project": "nexus",
                "session_id": "session-a",
                "turn_id": "turn-a",
                "gold_label": "accepted",
                "note": "",
            },
            {
                "sample_id": "b",
                "project": "vishnu",
                "session_id": "session-b",
                "turn_id": "turn-b",
                "gold_label": "",
                "note": "",
            },
        ]
        with self.assertRaisesRegex(ValueError, "missing or invalid gold label"):
            labels.score(manifest, review)
        review[1]["gold_label"] = "rejected"
        gold, matrix = labels.score(manifest, review)
        self.assertEqual(len(gold), 2)
        self.assertEqual(gold[1]["note"], "")
        self.assertEqual(matrix["accepted", "accepted"], 1)
        self.assertEqual(matrix["revised", "rejected"], 1)

        review[1]["turn_id"] = "another-turn"
        with self.assertRaisesRegex(ValueError, "turn_id differs"):
            labels.score(manifest, review)

    def test_read_candidates_excludes_unsettled_and_later_rows(self):
        with tempfile.TemporaryDirectory() as directory:
            db_path = Path(directory) / "crush.db"
            with sqlite3.connect(db_path) as conn:
                conn.executescript(
                    """CREATE TABLE turn_outcomes (
                           project_key TEXT, session_id TEXT, turn_id TEXT,
                           verdict TEXT, settled_at INTEGER
                       );
                       CREATE TABLE messages (id TEXT, session_id TEXT);"""
                )
                for name, settled in (("before", 100), ("after", 300), ("open", None)):
                    conn.execute(
                        "INSERT INTO turn_outcomes VALUES (?, ?, ?, ?, ?)",
                        ("project-key", name, name, "accepted", settled),
                    )
                    conn.execute("INSERT INTO messages VALUES (?, ?)", (name, name))
            rows = labels.read_candidates({"nexus": db_path}, 200)
            self.assertEqual([row["turn_id"] for row in rows], ["before"])

    def test_frozen_manifest_rejects_changed_sample(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "manifest.jsonl"
            path.write_text('{"sample_id":"changed"}\n')
            with self.assertRaisesRegex(ValueError, "pre-registered sample"):
                labels.verify_frozen_manifest(path)


if __name__ == "__main__":
    unittest.main()
