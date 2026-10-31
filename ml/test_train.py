import numpy as np
import pandas as pd

from train import pick_threshold, rules_alert, score, time_split


def test_time_split_keeps_order_and_never_overlaps():
    df = pd.DataFrame({"funded_at": range(100)})
    train, valid, test = time_split(df)
    assert len(train) == 60 and len(valid) == 20 and len(test) == 20
    assert (
        train["funded_at"].max()
        < valid["funded_at"].min()
        <= valid["funded_at"].max()
        < test["funded_at"].min()
    )


def test_score_counts_against_the_truth():
    y = np.array([1, 1, 0, 0, 1], bool)
    s = score(y, np.array([1, 0, 1, 0, 1], bool))
    assert s.alerts == 3 and s.precision == 2 / 3 and s.recall == 2 / 3


def test_threshold_keeps_the_precision_asked_for_with_the_most_recall():
    y = np.array([1, 1, 1, 0, 1, 0], bool)
    p = np.array([0.9, 0.8, 0.7, 0.6, 0.5, 0.1])
    t = pick_threshold(y, p, min_precision=0.8)
    assert t == 0.5  # 4 of 5 alerts right, all 4 rugs caught
    assert score(y, p >= t).precision >= 0.8


def test_threshold_falls_back_to_the_best_f1():
    y = np.array([0, 1, 0, 1], bool)
    p = np.array([0.9, 0.8, 0.7, 0.6])
    t = pick_threshold(y, p, min_precision=1.0)
    assert score(y, p >= t).f1 == max(score(y, p >= c).f1 for c in p)


def test_rules_alert_on_high_risk_and_scam_only():
    df = pd.DataFrame({"rules_level": ["clear", "caution", "high-risk", "scam"]})
    assert rules_alert(df).tolist() == [False, False, True, True]
