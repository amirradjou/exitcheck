"""Train and evaluate rugcheck's launch classifier (M4).

Reads the CSV that `rugcheck dataset` writes, splits it by time (train on the older launches, choose the alert
threshold on the next ones, test on the newest) and compares the model with the fixed rules on the same test
launches. The target is whether the launch's liquidity was pulled within a day. Each run is tracked in MLflow
(local: ml/mlflow.db and ml/mlruns).

usage: uv run train.py [data/dataset.csv] [--export]

Without a path it reads data/dataset.csv if `rugcheck dataset` wrote one, or else the published dataset.csv.gz, which
reproduces the numbers in the README. --export writes the model the Go code serves to internal/model/.
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
LABEL_HORIZON_S = (
    24 * 3600
)  # internal/outcome.Horizon: how far past its launch a label looks
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


def split_before(df: pd.DataFrame, end: int, test_n: int, valid_n: int):
    """Test on the test_n launches ending at index end, choose thresholds on the valid_n launches before them, train on
    all that came before those. No shuffling. A label looks a day past its launch, so each part stops a full day before
    the next begins: the model and its thresholds only use outcomes that were known by then, and the launches inside a
    gap are not used. Trimming a fixed split afterwards would not do, as a threshold window under a day long vanishes."""
    test = df.iloc[end - test_n : end]
    known = df[df["funded_at"] + LABEL_HORIZON_S <= test["funded_at"].min()]
    valid = known.iloc[-valid_n:]
    train = known[known["funded_at"] + LABEL_HORIZON_S <= valid["funded_at"].min()]
    return train, valid, test


def time_split(df: pd.DataFrame, valid: float = 0.2, test: float = 0.2):
    n = len(df)
    return split_before(df, n, int(n * test), int(n * valid))


def rules_alert(df: pd.DataFrame) -> np.ndarray:
    return df["rules_level"].isin(ALERT_LEVELS).to_numpy()


def fit(train: pd.DataFrame, valid: pd.DataFrame) -> XGBClassifier:
    model = XGBClassifier(**PARAMS)
    model.fit(
        features(train),
        train["pulled"].to_numpy(bool),
        eval_set=[(features(valid), valid["pulled"].to_numpy(bool))],
        verbose=False,
    )
    return model


def proba(model: XGBClassifier, df: pd.DataFrame) -> np.ndarray:
    return model.predict_proba(features(df))[:, 1]


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


def pick_grey_threshold(
    y: np.ndarray, p: np.ndarray, rules: np.ndarray, min_precision: float = 0.5
) -> float:
    """The threshold for launches the rules let through: the verdict is the rules first, the model only speaks where
    they are silent, and it can only add alerts. Of the thresholds whose added alerts are right more often than not
    (the reason shown to a reader says "usually"), the one where rules and model together have the best F1. If adding
    alerts never beats the rules alone, it adds none (a threshold above any score)."""
    grey = ~rules
    chosen, best = 1.01, score(y, rules).f1
    for t in np.unique(p[grey])[::-1]:
        added = score(y[grey], p[grey] >= t)
        together = score(y, rules | (p >= t)).f1
        if added.precision >= min_precision and together > best:
            chosen, best = float(t), together
    return chosen


def bootstrap_gap(
    y: np.ndarray, a: np.ndarray, b: np.ndarray, n: int = 2000, seed: int = 7
) -> dict[str, tuple[float, float, float]]:
    """How much better alerts a are than alerts b, resampling the same launches for both, with a 95% interval. A gap
    whose interval spans zero is not established by this test set, however good the point estimate looks."""
    rng = np.random.default_rng(seed)
    out: dict[str, list[float]] = {"precision": [], "recall": [], "f1": []}
    for _ in range(n):
        i = rng.integers(0, len(y), len(y))
        sa, sb = score(y[i], a[i]), score(y[i], b[i])
        out["precision"].append(sa.precision - sb.precision)
        out["recall"].append(sa.recall - sb.recall)
        out["f1"].append(sa.f1 - sb.f1)
    point = {
        "precision": score(y, a).precision - score(y, b).precision,
        "recall": score(y, a).recall - score(y, b).recall,
        "f1": score(y, a).f1 - score(y, b).f1,
    }
    return {
        k: (point[k], float(np.percentile(v, 2.5)), float(np.percentile(v, 97.5)))
        for k, v in out.items()
    }


def walk_forward(df: pd.DataFrame, folds: int = 3) -> list[dict]:
    """Repeat the whole procedure with the cut moving forward: one split could be a lucky one. Each fold trains on
    everything known before its threshold launches and tests on the launches right after them."""
    n, rows = len(df), []
    step = n // (folds + 3)
    for k in range(folds):
        train, valid, test = split_before(df, n - (folds - 1 - k) * step, step, step)
        if train["pulled"].nunique() < 2 or valid["pulled"].nunique() < 2:
            continue  # too little history before this cut to say anything
        model = fit(train, valid)
        y_valid, y_test = valid["pulled"].to_numpy(bool), test["pulled"].to_numpy(bool)
        rules_valid = rules_alert(valid)
        grey_t = pick_grey_threshold(y_valid, proba(model, valid), rules_valid)
        rules_test = rules_alert(test)
        p_test = proba(model, test)
        rows.append(
            {
                "train": len(train),
                "test": len(test),
                "rules": score(y_test, rules_test),
                "combined": score(y_test, rules_test | (p_test >= grey_t)),
                "added": int(((p_test >= grey_t) & ~rules_test).sum()),
            }
        )
    return rows


def table(rows: dict[str, Score]) -> str:
    out = ["| | alerts | precision | recall | F1 |", "|---|---|---|---|---|"]
    for name, s in rows.items():
        out.append(
            f"| {name} | {s.alerts} | {s.precision:.1%} | {s.recall:.1%} | {s.f1:.2f} |"
        )
    return "\n".join(out)


def main(path: Path, export: bool = False) -> None:
    df = load(path)
    train, valid, test = time_split(df)
    y = {
        k: d["pulled"].to_numpy(bool)
        for k, d in (("train", train), ("valid", valid), ("test", test))
    }
    model = fit(train, valid)
    p_valid, p_test = proba(model, valid), proba(model, test)

    rules_valid, rules_test_alert = rules_alert(valid), rules_alert(test)
    rules_valid_score = score(y["valid"], rules_valid)
    threshold = pick_threshold(y["valid"], p_valid, rules_valid_score.precision)
    grey_threshold = pick_grey_threshold(y["valid"], p_valid, rules_valid)
    model_alert = p_test >= threshold
    combined_alert = rules_test_alert | (p_test >= grey_threshold)
    model_test, rules_test = (
        score(y["test"], model_alert),
        score(y["test"], rules_test_alert),
    )
    combined_test = score(y["test"], combined_alert)

    def when(d: pd.DataFrame) -> str:
        t = pd.to_datetime(d["funded_at"], unit="s", utc=True)
        return f"{t.min():%Y-%m-%d %H:%M} to {t.max():%Y-%m-%d %H:%M} UTC"

    def mix(d: pd.DataFrame) -> str:
        return ", ".join(f"{k} {v}" for k, v in d["chain"].value_counts().items())

    def gap(name: str, alert: np.ndarray) -> str:
        g = bootstrap_gap(y["test"], alert, rules_test_alert)
        return (
            "; ".join(
                f"{k} {v[0]:+.1%} (95% interval {v[1]:+.1%} to {v[2]:+.1%})"
                for k, v in g.items()
                if k != "f1"
            )
            + f"; F1 {g['f1'][0]:+.3f} ({g['f1'][1]:+.3f} to {g['f1'][2]:+.3f}) [{name}]"
        )

    lines = [
        (
            f"launches: {len(df)} ({df['pulled'].mean():.0%} pulled within a day); "
            f"train {len(train)} ({when(train)}; {mix(train)}), threshold {len(valid)} ({when(valid)}; {mix(valid)}), "
            f"test {len(test)} ({when(test)}; {mix(test)})"
        ),
        (
            f"model-alone threshold {threshold:.3f} (the rules' precision on the threshold launches: "
            f"{rules_valid_score.precision:.1%}), threshold on what the rules let through {grey_threshold:.3f}; "
            f"model PR-AUC on test {average_precision_score(y['test'], p_test):.3f}"
        ),
        "",
        table(
            {
                "rules alone": rules_test,
                "model alone": model_test,
                "rules, model on what they let through": combined_test,
            }
        ),
        "",
        "against the rules alone, same launches resampled:",
        gap("model alone", model_alert),
        gap("rules + model", combined_alert),
    ]
    for name, m in [
        *(
            (str(d), (test["dex"] == d).to_numpy())
            for d in sorted(test["dex"].unique())
        ),
        *(
            (str(c), (test["chain"] == c).to_numpy())
            for c in sorted(test["chain"].unique())
        ),
    ]:
        lines += [
            "",
            f"{name} ({m.sum()} test launches, {y['test'][m].mean():.0%} pulled)",
            table(
                {
                    "rules alone": score(y["test"][m], rules_test_alert[m]),
                    "model alone": score(y["test"][m], model_alert[m]),
                    "rules + model": score(y["test"][m], combined_alert[m]),
                }
            ),
        ]
    folds = walk_forward(df)
    added = (p_test >= grey_threshold) & ~rules_test_alert
    lines += [
        "",
        (
            f"alerts added to the rules': {added.sum()}, of which pulled {y['test'][added].sum()}; "
            f"pulls the rules missed: {(y['test'] & ~rules_test_alert).sum()}"
        ),
        "",
        "walk-forward (the same procedure, cut earlier each time):",
        "| train | test | rules P / R / F1 | rules + model P / R / F1 | added alerts |",
        "|---|---|---|---|---|",
    ]
    for f in folds:
        r, c = f["rules"], f["combined"]
        lines.append(
            f"| {f['train']} | {f['test']} | {r.precision:.1%} / {r.recall:.1%} / {r.f1:.2f} "
            f"| {c.precision:.1%} / {c.recall:.1%} / {c.f1:.2f} | {f['added']} |"
        )
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
                "grey_threshold": grey_threshold,
                "rows": len(df),
                "best_iteration": model.best_iteration,
            }
        )
        for name, s in (
            ("model", model_test),
            ("rules", rules_test),
            ("combined", combined_test),
        ):
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

    if export:
        export_model(model, df, grey_threshold, rules_test, combined_test, p_test, test)


def export_model(
    model: XGBClassifier,
    df: pd.DataFrame,
    threshold: float,
    rules: Score,
    combined: Score,
    p_test: np.ndarray,
    test: pd.DataFrame,
) -> None:
    """What the Go code serves: the trees, the threshold, and a few test launches with their scores, so a test can hold
    the Go scorer to XGBoost's own numbers."""
    dest = HERE.parent / "internal" / "model"
    dest.mkdir(exist_ok=True)
    model.save_model(dest / "model.json")
    with (dest / "model.json").open("a") as f:
        f.write(
            "\n"
        )  # XGBoost writes none, and the end-of-file hook would otherwise change the file on every export
    (dest / "meta.json").write_text(
        json.dumps(
            {
                "threshold": threshold,
                "features": list(features(df).columns),
                "trained_rows": len(df),
                "test": {
                    "rules_precision": rules.precision,
                    "rules_recall": rules.recall,
                    "precision": combined.precision,
                    "recall": combined.recall,
                },
            },
            indent=1,
        )
        + "\n"
    )
    sample = test.iloc[:: max(1, len(test) // 40)]
    rows = [
        {
            "id": row["id"],
            "features": [
                None if pd.isna(v) else float(v)
                for v in features(sample.loc[[i]]).iloc[0]
            ],
            "score": float(proba(model, sample.loc[[i]])[0]),
        }
        for i, row in sample.iterrows()
    ]
    (dest / "testdata").mkdir(exist_ok=True)
    (dest / "testdata" / "golden.json").write_text(json.dumps(rows, indent=0) + "\n")


def default_path() -> Path:
    built = HERE / "data" / "dataset.csv"
    return built if built.exists() else HERE / "dataset.csv.gz"


if __name__ == "__main__":
    args = [a for a in sys.argv[1:] if a != "--export"]
    main(Path(args[0]) if args else default_path(), "--export" in sys.argv)
