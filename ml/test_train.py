import numpy as np
import pandas as pd

from train import (
    LABEL_HORIZON_S,
    bootstrap_gap,
    pick_grey_threshold,
    pick_threshold,
    rules_alert,
    score,
    time_split,
)


def test_time_split_keeps_order_and_leaves_a_label_horizon_between_its_parts():
    # One launch an hour for 10 days. Every training label must be known before the threshold launches begin, and every
    # threshold label before the test launches begin.
    df = pd.DataFrame({"funded_at": [i * 3600 for i in range(240)]})
    train, valid, test = time_split(df)
    assert len(test) == 48 and len(valid) == 48
    assert test["funded_at"].min() == 192 * 3600
    assert valid["funded_at"].max() + LABEL_HORIZON_S == test["funded_at"].min()
    assert train["funded_at"].max() + LABEL_HORIZON_S == valid["funded_at"].min()
    assert len(train) == 98


def test_a_short_threshold_window_is_not_trimmed_away():
    # Many launches in the last hours, as when a second chain's data starts late: the windows are moved, not emptied.
    old = [i * 3600 for i in range(100)]
    new = [100 * 3600 + i * 60 for i in range(100)]
    df = pd.DataFrame({"funded_at": old + new})
    train, valid, test = time_split(df)
    assert len(valid) == 40 and len(train) > 0
    assert valid["funded_at"].max() + LABEL_HORIZON_S <= test["funded_at"].min()


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


def test_grey_threshold_only_looks_at_what_the_rules_let_through():
    y = np.array([0, 0, 1, 1, 0, 1, 1, 1, 1], bool)
    rules = np.array([1, 1, 0, 0, 0, 1, 1, 1, 1], bool)
    # The two highest scores are alerts the rules already sent, both wrong. They must not spoil the threshold.
    p = np.array([0.99, 0.98, 0.7, 0.6, 0.2, 0.9, 0.9, 0.9, 0.9])
    t = pick_grey_threshold(y, p, rules)
    assert t == 0.6
    assert score(y[~rules], p[~rules] >= t).recall == 1.0


def test_grey_threshold_adds_nothing_when_added_alerts_would_be_mostly_wrong():
    y = np.array([0, 0, 0, 1, 1], bool)
    rules = np.array([0, 0, 0, 0, 1], bool)
    p = np.array([0.9, 0.8, 0.7, 0.6, 0.1])
    assert (p[~rules] >= pick_grey_threshold(y, p, rules)).sum() == 0


def test_grey_threshold_adds_nothing_when_the_rules_cannot_be_beaten():
    y = np.array([1, 1, 0], bool)
    rules = np.array([1, 1, 0], bool)
    p = np.array([0.9, 0.8, 0.7])
    assert (p[~rules] >= pick_grey_threshold(y, p, rules)).sum() == 0


def test_bootstrap_gap_is_zero_for_identical_alerts_and_brackets_the_point():
    y = np.array([1, 0, 1, 0, 1, 1, 0, 1] * 5, bool)
    a = np.array([1, 0, 1, 0, 1, 0, 0, 1] * 5, bool)
    assert bootstrap_gap(y, a, a, n=100)["f1"] == (0.0, 0.0, 0.0)
    b = np.array([1, 1, 0, 0, 1, 0, 1, 0] * 5, bool)
    point, low, high = bootstrap_gap(y, a, b, n=200)["f1"]
    assert low <= point <= high and point > 0
