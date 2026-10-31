"""Train and evaluate rugcheck's launch classifier (M4).

Reads the CSV that `rugcheck dataset` writes, splits it by time (train on the older launches, choose the alert
threshold on the next ones, test on the newest) and compares the model with the fixed rules on the same test
launches. The target is whether the launch's liquidity was pulled within a day. Each run is tracked in MLflow
(local: ml/mlflow.db and ml/mlruns).

usage: uv run train.py [data/dataset.csv]
"""

import json
import sys
from dataclasses import dataclass
from pathlib import Path

import mlflow
import numpy as np
import pandas as pd
from sklearn.metrics import average_precision_score
from xgboost import XGBClassifier

HERE = Path(__file__).parent
META = [
    "id",
    "chain",
    "dex",
    "funded_at",
    "source",
    "rules_level",
    "live_level",
    "pulled",
    "bait",
    "buys",
    "sells",
    "minutes_to_pull",
]
ALERT_LEVELS = {"high-risk", "scam"}
PARAMS = {
    "n_estimators": 600,
    "learning_rate": 0.05,
    "max_depth": 4,
    "min_child_weight": 2,
    "subsample": 0.8,
    "colsample_bytree": 0.8,
    "eval_metric": "aucpr",
    "early_stopping_rounds": 50,
    "random_state": 7,
}


def load(path: Path) -> pd.DataFrame:
    df = pd.read_csv(path)
    return df.sort_values("funded_at", kind="stable").reset_index(drop=True)


def features(df: pd.DataFrame) -> pd.DataFrame:
    return df.drop(columns=META)


def time_split(df: pd.DataFrame, valid: float = 0.2, test: float = 0.2):
    """Oldest launches to train, the next ones to choose the threshold, the newest to test. No shuffling: the
    model is judged on launches that came after everything it learned from."""
    n = len(df)
    a, b = int(n * (1 - valid - test)), int(n * (1 - test))
    return df.iloc[:a], df.iloc[a:b], df.iloc[b:]


def rules_alert(df: pd.DataFrame) -> np.ndarray:
    return df["rules_level"].isin(ALERT_LEVELS).to_numpy()


@dataclass
class Score:
    alerts: int
    precision: float
    recall: float

    @property
    def f1(self) -> float:
        p, r = self.precision, self.recall
        return 2 * p * r / (p + r) if p + r else 0.0


def score(y: np.ndarray, alert: np.ndarray) -> Score:
    y, alert = np.asarray(y, bool), np.asarray(alert, bool)
    tp = int((y & alert).sum())
    return Score(
        int(alert.sum()),
        tp / alert.sum() if alert.sum() else 0.0,
        tp / y.sum() if y.sum() else 0.0,
    )


def pick_threshold(y: np.ndarray, p: np.ndarray, min_precision: float) -> float:
    """The lowest threshold (most recall) whose precision on these launches is at least min_precision; if none
    reaches it, the one with the best F1."""
    candidates = np.unique(p)[::-1]
    best, best_f1 = 0.5, -1.0
    chosen = None
    for t in candidates:
        s = score(y, p >= t)
        if s.precision >= min_precision:
            chosen = t
        if s.f1 > best_f1:
            best, best_f1 = t, s.f1
    return float(chosen if chosen is not None else best)


def table(rows: dict[str, Score]) -> str:
    out = ["| | alerts | precision | recall | F1 |", "|---|---|---|---|---|"]
    for name, s in rows.items():
        out.append(
            f"| {name} | {s.alerts} | {s.precision:.1%} | {s.recall:.1%} | {s.f1:.2f} |"
        )
    return "\n".join(out)


def main(path: Path) -> None:
    df = load(path)
    train, valid, test = time_split(df)
    y = {
        k: d["pulled"].to_numpy(bool)
        for k, d in (("train", train), ("valid", valid), ("test", test))
    }
    model = XGBClassifier(**PARAMS)
    model.fit(
        features(train),
        y["train"],
        eval_set=[(features(valid), y["valid"])],
        verbose=False,
    )
    p_valid = model.predict_proba(features(valid))[:, 1]
    p_test = model.predict_proba(features(test))[:, 1]

    rules_valid = score(y["valid"], rules_alert(valid))
    threshold = pick_threshold(y["valid"], p_valid, rules_valid.precision)
    model_test, rules_test = (
        score(y["test"], p_test >= threshold),
        score(y["test"], rules_alert(test)),
    )

    def when(d: pd.DataFrame) -> str:
        t = pd.to_datetime(d["funded_at"], unit="s", utc=True)
        return f"{t.min():%Y-%m-%d %H:%M} to {t.max():%Y-%m-%d %H:%M} UTC"

    lines = [
        (
            f"launches: {len(df)} ({df['pulled'].mean():.0%} pulled within a day); "
            f"train {len(train)} ({when(train)}), threshold {len(valid)} ({when(valid)}), test {len(test)} ({when(test)})"
        ),
        (
            f"alert threshold {threshold:.3f} (the rules' precision on the threshold launches: {rules_valid.precision:.1%}); "
            f"model PR-AUC on test {average_precision_score(y['test'], p_test):.3f}"
        ),
        "",
        table({"rules alone": rules_test, "model": model_test}),
    ]
    for dex in sorted(test["dex"].unique()):
        m = (test["dex"] == dex).to_numpy()
        lines += [
            "",
            f"{dex} ({m.sum()} test launches, {y['test'][m].mean():.0%} pulled)",
            table(
                {
                    "rules alone": score(y["test"][m], rules_alert(test)[m]),
                    "model": score(y["test"][m], (p_test >= threshold)[m]),
                }
            ),
        ]
    report = "\n".join(lines)
    print(report)

    importance = dict(
        sorted(
            zip(features(df).columns, model.feature_importances_.tolist()),
            key=lambda kv: -kv[1],
        )
    )
    mlflow.set_tracking_uri(f"sqlite:///{HERE / 'mlflow.db'}")
    mlflow.set_experiment("rugcheck-m4")
    with mlflow.start_run():
        mlflow.log_params(
            {
                **PARAMS,
                "threshold": threshold,
                "rows": len(df),
                "best_iteration": model.best_iteration,
            }
        )
        for name, s in (("model", model_test), ("rules", rules_test)):
            mlflow.log_metrics(
                {
                    f"test_{name}_precision": s.precision,
                    f"test_{name}_recall": s.recall,
                    f"test_{name}_f1": s.f1,
                    f"test_{name}_alerts": s.alerts,
                }
            )
        mlflow.log_metric(
            "test_model_pr_auc", average_precision_score(y["test"], p_test)
        )
        out = HERE / "out"
        out.mkdir(exist_ok=True)
        model.save_model(out / "model.json")
        (out / "importance.json").write_text(json.dumps(importance, indent=1))
        (out / "results.md").write_text(report + "\n")
        mlflow.log_artifacts(str(out))


if __name__ == "__main__":
    main(Path(sys.argv[1]) if len(sys.argv) > 1 else HERE / "data" / "dataset.csv")
