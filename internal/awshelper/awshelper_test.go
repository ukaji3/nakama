package awshelper

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEC2Tags_DeterministicOrder(t *testing.T) {
	t.Parallel()

	in := map[string]string{
		"b": "2",
		"a": "1",
		"c": "3",
	}
	got := EC2Tags(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 tags, got %d", len(got))
	}
	want := []string{"a", "b", "c"}
	for i, k := range want {
		if got[i].Key == nil || *got[i].Key != k {
			t.Errorf("tag[%d].Key=%v, want %s", i, got[i].Key, k)
		}
	}
}

func TestEC2MapTags_RoundTrip(t *testing.T) {
	t.Parallel()

	in := map[string]string{
		"nakama:managed": "true",
		"Name":           "nakama-vpc",
	}
	round := EC2MapTags(EC2Tags(in))
	if len(round) != len(in) {
		t.Fatalf("round trip changed cardinality: in=%d out=%d", len(in), len(round))
	}
	for k, v := range in {
		if round[k] != v {
			t.Errorf("round[%q]=%q, want %q", k, round[k], v)
		}
	}
}

func TestPoll_ImmediateSuccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	calls := 0
	err := Poll(ctx, PollOptions{Interval: 1 * time.Millisecond, Timeout: time.Second}, func(_ context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestPoll_AfterRetries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	calls := 0
	err := Poll(ctx, PollOptions{Interval: 5 * time.Millisecond, Timeout: time.Second}, func(_ context.Context) (bool, error) {
		calls++
		return calls >= 3, nil
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 calls, got %d", calls)
	}
}

func TestPoll_Timeout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	err := Poll(ctx, PollOptions{Interval: 5 * time.Millisecond, Timeout: 30 * time.Millisecond, Description: "vpc available"}, func(_ context.Context) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("expected ErrPollTimeout, got %v", err)
	}
}

func TestPoll_PropagatesError(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond, Timeout: time.Second}, func(_ context.Context) (bool, error) {
		return false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestPoll_RespectsContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Poll(ctx, PollOptions{Interval: 5 * time.Millisecond, Timeout: time.Second}, func(_ context.Context) (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("expected non-nil error after canceled context")
	}
}
