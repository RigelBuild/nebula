package main

import (
	"reflect"
	"testing"
)

const liveLogin = "76635     1 /usr/bin/login -f -pq -h 100.64.0.1 exampleuser /bin/zsh -c sleep 900\n"

func TestSelectTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ps   string
		want []int
	}{
		{"live repro", liveLogin + "76638 76635 sleep 900\n", []int{76638}},
		{"grandchild", liveLogin + "76638 76635 /bin/zsh -c x\n76640 76638 sleep 900\n", []int{76638, 76640}},
		{"healthy login", "76635 500 /usr/bin/login -f -pq -h 100.64.0.1 m /bin/zsh\n76638 76635 sleep 900\n", nil},
		{"no -h", "76635 1 /usr/bin/login -flp m /bin/zsh\n76638 76635 sleep 900\n", nil},
		{"private ip", "76635 1 /usr/bin/login -f -h 10.0.0.5 m\n76638 76635 sleep 900\n", nil},
		{"outside cgnat", "76635 1 /usr/bin/login -f -h 203.0.113.1 m\n76638 76635 sleep 900\n", nil},
		{"just above cgnat", "76635 1 /usr/bin/login -f -h 100.128.0.1 m\n76638 76635 sleep 900\n", nil},
		{"garbage ip", "76635 1 /usr/bin/login -f -h not.an.ip m\n76638 76635 sleep 900\n", nil},
		{"ppid1 non-login", "76635 1 /usr/sbin/cron -h 100.64.0.1\n76638 76635 sleep 900\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []int
			for _, tg := range selectTargets(parsePS(tt.ps)) {
				if tg.LoginPID != 76635 {
					t.Errorf("pid %d login %d, want 76635", tg.PID, tg.LoginPID)
				}
				got = append(got, tg.PID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("targets = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectTargetsRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ps   string
		want []int
	}{
		{"activate-rs tree skipped", liveLogin + "76638 76635 /nix/store/abc-bash-5.3/bin/bash /nix/store/def-activatable-darwin-system/activate-rs activate /nix/store/x --temp-path /tmp\n76639 76638 /nix/store/ghi-deploy-rs-0.1.0/bin/activate activate /nix/store/x\n76640 76639 /run/current-system/sw/bin/darwin-rebuild switch\n76641 76635 sleep 900\n", []int{76641}},
		{"live -p -h form", "76635 1 /usr/bin/login -f -p -h 100.64.0.9 exampleuser /bin/zsh -c x\n76638 76635 sleep 900\n", []int{76638}},
		{"-h inside command", "76635 1 /usr/bin/login -pfl m /bin/zsh -c \"foo -h 100.64.0.9\"\n76638 76635 sleep 900\n", nil},
		{"-h after username", "76635 1 /usr/bin/login -pfl m -h 100.64.0.9\n76638 76635 sleep 900\n", nil},
		{"post-exec activate skipped", liveLogin + "76638 76635 /nix/store/ghi-deploy-rs-0.1.0/bin/activate activate /nix/store/x --magic-rollback\n76639 76638 sleep 905\n76640 76635 sleep 900\n", []int{76640}},
		{"bare activate targeted", liveLogin + "76638 76635 /usr/bin/activate activate /nix/store/x\n", []int{76638}},
		{"notactivate-rs script targeted", liveLogin + "76638 76635 bash /tmp/notactivate-rs.sh\n", []int{76638}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []int
			for _, tg := range selectTargets(parsePS(tt.ps)) {
				if tg.LoginPID != 76635 {
					t.Errorf("pid %d login %d, want 76635", tg.PID, tg.LoginPID)
				}
				got = append(got, tg.PID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("targets = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoginOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ps   string
		pid  int
		want int
	}{
		{"direct child", liveLogin + "76638 76635 sleep 900\n", 76638, 76635},
		{"ppid cycle", "76638 76640 sleep 900\n76640 76638 /bin/zsh\n", 76638, 0},
		{"self cycle", "76638 76638 sleep 900\n", 76638, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := loginOf(parsePS(tt.ps), tt.pid); got != tt.want {
				t.Errorf("loginOf = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRecheck(t *testing.T) {
	t.Parallel()
	targets := selectTargets(parsePS(liveLogin + "76638 76635 sleep 900\n76639 76635 sleep 901\n76640 76635 sleep 902\n"))
	tests := []struct {
		name string
		ps   string
		want []int
	}{
		{"all alive", liveLogin + "76638 76635 sleep 900\n76639 76635 sleep 901\n76640 76635 sleep 902\n", []int{76638, 76639, 76640}},
		{"command changed and gone", liveLogin + "76638 76635 sleep 900\n76639 76635 other\n", []int{76638}},
		{"login gone", "76638 1 sleep 900\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []int
			for _, tg := range recheck(targets, parsePS(tt.ps)) {
				got = append(got, tg.PID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("recheck = %v, want %v", got, tt.want)
			}
		})
	}
}
