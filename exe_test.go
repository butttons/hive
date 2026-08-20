package main

import "testing"

func TestExeVMName(t *testing.T) {
	cases := []struct {
		server  string
		want    string
		wantErr bool
	}{
		{"counter-butttons.exe.xyz", "counter-butttons", false},
		{"exedev@counter-butttons.exe.xyz", "counter-butttons", false},
		{"mac-mini.local", "", true},
		{"user@192.168.1.10", "", true},
		{".exe.xyz", "", true},
		{"Bad_Name.exe.xyz", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		app := &App{Hive: HiveConfig{Server: c.server}}
		got, err := exeVMName(app)
		if c.wantErr {
			if err == nil {
				t.Errorf("exeVMName(%q): want error, got %q", c.server, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("exeVMName(%q): %v", c.server, err)
			continue
		}
		if got != c.want {
			t.Errorf("exeVMName(%q) = %q, want %q", c.server, got, c.want)
		}
	}
}
