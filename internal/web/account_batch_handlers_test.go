package web

import (
	"errors"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
)

func TestAccountBatchDetectResultMessageExplainsFailures(t *testing.T) {
	result := controlplane.AccountBatchResult{
		Action:    controlplane.AccountBatchActionTest,
		Total:     2,
		Succeeded: 1,
		Failed:    1,
		Items: []controlplane.AccountBatchItemResult{
			{AccountID: "acct_ok", Label: "Main", Status: "ok", Message: "ping ok status=active"},
			{AccountID: "acct_bad", Label: "Backup", Status: "failed", Message: "upstream_transport_error: proxy timeout"},
		},
	}

	message := (&Server{}).accountBatchResultMessage(localeZH, result)
	for _, want := range []string{
		"\u6279\u91cf\u68c0\u6d4b\u5b8c\u6210\uff1a1 \u4e2a\u901a\u8fc7\uff0c1 \u4e2a\u672a\u901a\u8fc7\uff0c0 \u4e2a\u8df3\u8fc7\u3002",
		"\u901a\u8fc7\uff1a\n- Main",
		"\u672a\u901a\u8fc7\uff1a\n- Backup - proxy timeout",
		"\u68c0\u6d4b\u672a\u901a\u8fc7\u7684\u8d26\u53f7\u5df2\u6807\u4e3a\u5f02\u5e38\u3002",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q missing %q", message, want)
		}
	}
	if strings.Contains(message, "upstream_transport_error") {
		t.Fatalf("message should hide raw upstream code: %q", message)
	}
}

func TestAccountBatchDetectResultMessageAllPassed(t *testing.T) {
	result := controlplane.AccountBatchResult{
		Action:    controlplane.AccountBatchActionTest,
		Total:     2,
		Succeeded: 2,
		Items: []controlplane.AccountBatchItemResult{
			{AccountID: "acct_a", Status: "ok"},
			{AccountID: "acct_b", Status: "ok"},
		},
	}

	message := (&Server{}).accountBatchResultMessage(localeEN, result)
	for _, want := range []string{"Detection complete: 2 passed, 0 failed, 0 skipped.", "Passed:\n- acct_a\n- acct_b", "No account status action is needed."} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q missing %q", message, want)
		}
	}
}

func TestAccountBatchDetectResultMessageDoesNotHideSmallPassList(t *testing.T) {
	result := controlplane.AccountBatchResult{
		Action:    controlplane.AccountBatchActionTest,
		Total:     5,
		Succeeded: 5,
		Items: []controlplane.AccountBatchItemResult{
			{AccountID: "acct_a", Label: "A", Status: "ok"},
			{AccountID: "acct_b", Label: "B", Status: "ok"},
			{AccountID: "acct_c", Label: "C", Status: "ok"},
			{AccountID: "acct_d", Label: "D", Status: "ok"},
			{AccountID: "acct_e", Label: "E", Status: "ok"},
		},
	}

	message := (&Server{}).accountBatchResultMessage(localeZH, result)
	if !strings.Contains(message, "\u901a\u8fc7\uff1a\n- A\n- B\n- C\n- D\n- E") {
		t.Fatalf("message should list all five passed accounts: %q", message)
	}
	if strings.Contains(message, "\u7b49 1 \u4e2a") || strings.Contains(message, "\u672a\u663e\u793a") {
		t.Fatalf("message should not hide one account: %q", message)
	}
}

func TestAccountBatchDetectResultMessageListsSkippedAccounts(t *testing.T) {
	result := controlplane.AccountBatchResult{
		Action:    controlplane.AccountBatchActionTest,
		Total:     3,
		Succeeded: 1,
		Skipped:   2,
		Items: []controlplane.AccountBatchItemResult{
			{AccountID: "acct_ok", Label: "Main", Status: "ok"},
			{AccountID: "acct_skip_1", Label: "Skip A", Status: "skipped"},
			{AccountID: "acct_skip_2", Label: "Skip B", Status: "skipped"},
		},
	}

	message := (&Server{}).accountBatchResultMessage(localeEN, result)
	for _, want := range []string{"Skipped:\n- Skip A\n- Skip B", "Skipped accounts were not changed."} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q missing %q", message, want)
		}
	}
}

func TestAccountBatchNonDetectResultUsesGenericMessage(t *testing.T) {
	result := controlplane.AccountBatchResult{
		Action:    controlplane.AccountBatchActionRefreshQuota,
		Total:     2,
		Succeeded: 2,
	}

	message := (&Server{}).accountBatchResultMessage(localeEN, result)
	if !strings.Contains(message, "Refresh Quota done: 2 succeeded, 0 failed, 0 skipped.") {
		t.Fatalf("message = %q", message)
	}
}

func TestAccountBatchJobPayloadLocalizesRunningProgress(t *testing.T) {
	queued := (&Server{}).accountBatchJobPayload(localeEN, controlplane.AccountBatchJobSnapshot{
		ID:     "acctbatch_queued",
		Action: controlplane.AccountBatchActionRefreshQuota,
		Status: controlplane.AccountBatchJobQueued,
		Total:  2,
	})
	if message, _ := queued["message"].(string); !strings.Contains(message, "Refresh Quota task is starting for 2 accounts.") {
		t.Fatalf("queued message = %q", message)
	}

	payload := (&Server{}).accountBatchJobPayload(localeEN, controlplane.AccountBatchJobSnapshot{
		ID:        "acctbatch_test",
		Action:    controlplane.AccountBatchActionTest,
		Status:    controlplane.AccountBatchJobRunning,
		Total:     5,
		Done:      2,
		Succeeded: 1,
		Failed:    1,
		Current:   "acct_c",
	})

	if payload["state"] != string(controlplane.AccountBatchJobRunning) {
		t.Fatalf("state = %#v", payload["state"])
	}
	for _, want := range []string{"Detect running: 2/5 done.", "Passed 1 / Failed 1 / Skipped 0", "Current: acct_c"} {
		joined := payload["message"].(string) + " " + payload["counts_text"].(string) + " " + payload["current_text"].(string)
		if !strings.Contains(joined, want) {
			t.Fatalf("payload text %q missing %q", joined, want)
		}
	}
}

func TestAccountBatchJobPayloadIncludesMoveGroupTarget(t *testing.T) {
	payload := (&Server{}).accountBatchJobPayload(localeEN, controlplane.AccountBatchJobSnapshot{
		ID:          "acctbatch_move",
		Action:      controlplane.AccountBatchActionMoveGroup,
		TargetGroup: "Plus",
		Status:      controlplane.AccountBatchJobCompleted,
		Total:       2,
		Done:        2,
		Succeeded:   2,
	})

	if payload["target_group"] != "Plus" {
		t.Fatalf("target_group = %#v, want Plus", payload["target_group"])
	}
}

func TestAccountCheckReasonTextExplainsTimeout(t *testing.T) {
	got := accountCheckReasonText(localeZH, "context deadline exceeded")
	if !strings.Contains(got, "\u68c0\u6d4b\u8d85\u65f6") {
		t.Fatalf("timeout reason = %q", got)
	}
}

func TestAccountCheckReasonTextStripsTemporaryUnavailablePrefix(t *testing.T) {
	got := accountCheckReasonText(localeZH, "upstream_temporarily_unavailable: no available channel")
	if strings.Contains(got, "upstream_temporarily_unavailable") {
		t.Fatalf("reason should hide raw code: %q", got)
	}
	if !strings.Contains(got, "no available channel") {
		t.Fatalf("reason = %q", got)
	}
}

func TestAccountBatchJobErrorMessageLocalizesCommonErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("select at least one account"), "\u8bf7\u5148\u9009\u62e9\u81f3\u5c11\u4e00\u4e2a\u8d26\u53f7\uff0c\u518d\u5f00\u59cb\u6279\u91cf\u4efb\u52a1\u3002"},
		{errors.New("account batch job acctbatch_test is already running"), "\u5df2\u6709\u6279\u91cf\u4efb\u52a1\u5728\u8fd0\u884c"},
		{errors.New("batch action refresh_quota supports at most 500 accounts at a time"), "\u5237\u65b0\u914d\u989d\u4e00\u6b21\u6700\u591a\u5904\u7406 500 \u4e2a\u8d26\u53f7"},
	}
	for _, tc := range cases {
		got := accountBatchJobErrorMessage(localeZH, tc.err)
		if !strings.Contains(got, tc.want) {
			t.Fatalf("message %q missing %q", got, tc.want)
		}
	}
}
