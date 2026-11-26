"""Exercise recovery rendering without installing UI dependencies."""
import json
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


def _node(script, data):
    if not shutil.which("node"):
        pytest.skip("node unavailable")
    proc = subprocess.run(["node", "-e", script], input=json.dumps(data), text=True,
                          cwd=ROOT, capture_output=True, timeout=30)
    assert proc.returncode == 0, proc.stderr
    result = json.loads(proc.stdout)
    if isinstance(result, dict) and result.get("skip"):
        pytest.skip(result["skip"])
    return result


@pytest.mark.parametrize("legacy", [False, True])
def test_dashboard_displays_compactions_and_recovery_without_loss_or_score_bar(legacy):
    bd = ({"compaction_depth": {"compactions": 2, "score": 10, "detail": "~88% lost"}}
          if legacy else {"compactions": {"count": 2, "weighted": False},
                          "compaction_recovery": {"status": "needs_attention", "score": 10}})
    html = _node(r"""
const fs = require('node:fs'), vm = require('node:vm');
const q = {breakdown: JSON.parse(fs.readFileSync(0, 'utf8'))};
const source = fs.readFileSync('skills/token-optimizer/assets/dashboard.html', 'utf8');
const block = source.split('// Breakdown bars')[1].split('// Waste estimate')[0];
const html = vm.runInNewContext('var html="";\n' + block + '\nhtml', {q, esc:String});
process.stdout.write(JSON.stringify(html));
""", bd)
    assert "Compactions" in html and "2 compactions" in html
    assert "Recovery" in html and "observing" in html
    assert "lost" not in html and "60-70" not in html and "needs_attention" not in html
    assert "transition:width" not in html


def test_published_compaction_examples_do_not_claim_fixed_loss():
    terms = ["~65%", "88%", "60-70%", "30% remains", "context: lost"]
    for base in [ROOT / "skills/token-optimizer/assets"]:
        for name in ["quality-example.svg", "status-bar.svg", "hero-terminal.svg", "user-profiles.svg"]:
            text = (base / name).read_text(encoding="utf-8")
            assert not any(term in text for term in terms), name
