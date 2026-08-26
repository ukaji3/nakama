package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyLicense(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		want    string
		wantErr bool
	}{
		{
			name: "full Apache-2.0 text",
			text: "                                 Apache License\n" +
				"                           Version 2.0, January 2004\n" +
				"                        http://www.apache.org/licenses/\n",
			want: "Apache License 2.0",
		},
		{
			name: "short Apache-2.0 grant notice (jmespath style)",
			text: "Copyright 2015 Example Author\n\n" +
				"Licensed under the Apache License, Version 2.0 (the \"License\");\n" +
				"you may not use this file except in compliance with the License.\n",
			want: "Apache License 2.0",
		},
		{
			name: "MIT",
			text: "MIT License\n\nCopyright (c) 2014 example\n\n" +
				"Permission is hereby granted, free of charge, to any person obtaining a copy\n",
			want: "MIT License",
		},
		{
			name: "BSD 2-Clause (gorilla style)",
			text: "Copyright (c) 2013 Example Authors. All rights reserved.\n\n" +
				"Redistribution and use in source and binary forms, with or without\n" +
				"modification, are permitted provided that the following conditions are met:\n\n" +
				"  Redistributions of source code must retain the above copyright notice...\n" +
				"  Redistributions in binary form must reproduce the above copyright notice...\n",
			want: "BSD 2-Clause License",
		},
		{
			name: "BSD 3-Clause (endorse-or-promote marker)",
			text: "Copyright 2009 The Example Authors.\n\n" +
				"Redistribution and use in source and binary forms, with or without\n" +
				"modification, are permitted provided that the following conditions are met:\n" +
				"   * Neither the name of Google LLC nor the names of its\n" +
				"contributors may be used to endorse or promote products derived from\n" +
				"this software without specific prior written permission.\n",
			want: "BSD 3-Clause License",
		},
		{
			name: "BSD 3-Clause with CRLF line endings (seelog style)",
			text: "Copyright (c) 2012, Example Co.\r\nAll rights reserved.\r\n" +
				"Redistribution and use in source and binary forms, with or without\r\n" +
				"    * Neither the name of the copyright holder nor the names of its contributors\r\n",
			want: "BSD 3-Clause License",
		},
		{
			name: "BSD 4-Clause is surfaced distinctly",
			text: "Redistribution and use in source and binary forms, with or without\n" +
				"3. All advertising materials mentioning features or use of this software...\n" +
				"4. Neither the name of the organization nor the names of its contributors...\n",
			want: "BSD 4-Clause License",
		},
		{
			name: "yaml.v3 dual license",
			text: "This project is covered by two different licenses: MIT and Apache.\n\n" +
				"#### MIT License ####\n...\n#### Apache License ####\n" +
				"Licensed under the Apache License, Version 2.0\n" +
				"Permission is hereby granted, free of charge, ...\n",
			want: "MIT License and Apache License 2.0",
		},
		{
			name:    "unknown license fails loudly",
			text:    "You may use this software under the Example Public License v42.\n",
			wantErr: true,
		},
		{
			name:    "GPL-shaped text is not silently classified",
			text:    "This program is free software: you can redistribute it and/or modify it under the terms of the GNU General Public License.\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := classifyLicense(tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("classifyLicense = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyLicense: %v", err)
			}
			if got != tc.want {
				t.Errorf("classifyLicense = %q, want %q", got, tc.want)
			}
		})
	}
}

// writeModuleDir creates a fake module directory holding the given
// files and returns its path.
func writeModuleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const (
	fakeApache = "                                 Apache License\n" +
		"                           Version 2.0, January 2004\n" +
		"(full text elided for the fixture)\n"
	fakeMIT = "MIT License\n\nPermission is hereby granted, free of charge, to any person...\n"
	fakeBSD = "Copyright 2009 The Example Authors.\n" +
		"Redistribution and use in source and binary forms, with or without\n" +
		"neither the name of the copyright holder nor the names of its contributors\n"
)

func TestBuildNotices_GroupingAndRendering(t *testing.T) {
	t.Parallel()
	// Two modules share the identical Apache text (one carries a
	// NOTICE), one module is MIT with a lexicographically earlier
	// path, exercising grouping, NOTICE reproduction and ordering.
	apacheDir1 := writeModuleDir(t, map[string]string{
		"LICENSE.txt": fakeApache,
		"NOTICE.txt":  "Example Product\nCopyright 2015 Example, Inc.\n",
	})
	apacheDir2 := writeModuleDir(t, map[string]string{"LICENSE.txt": fakeApache})
	mitDir := writeModuleDir(t, map[string]string{"LICENSE": fakeMIT})

	mods := []moduleInfo{
		{Path: "example.com/zzz/sdk", Version: "v1.2.3", Dir: apacheDir1},
		{Path: "example.com/zzz/sdk/sub", Version: "v0.9.0", Dir: apacheDir2},
		{Path: "example.com/aaa/tool", Version: "v2.0.0", Dir: mitDir},
	}
	got, err := buildNotices("Copyright 2009 The Go Authors.\n(stdlib text)\n", mods)
	if err != nil {
		t.Fatalf("buildNotices: %v", err)
	}

	// Header and stdlib section come first, without any
	// environment-specific values.
	if !strings.HasPrefix(got, "THIRD-PARTY NOTICES for nakama\n") {
		t.Errorf("missing header prefix")
	}
	stdlibIdx := strings.Index(got, "Go standard library and runtime — BSD 3-Clause License")
	if stdlibIdx < 0 {
		t.Fatalf("stdlib section missing")
	}
	for _, banned := range []string{"go1.", "Generated at nakama v"} {
		if strings.Contains(got, banned) {
			t.Errorf("output contains environment-specific value %q", banned)
		}
	}

	// MIT group (aaa) sorts before the Apache group (zzz).
	mitIdx := strings.Index(got, "example.com/aaa/tool — MIT License")
	apacheIdx := strings.Index(got, "example.com/zzz/sdk and related modules (2 modules) — Apache License 2.0")
	if mitIdx < 0 || apacheIdx < 0 {
		t.Fatalf("expected section titles missing:\nmit=%d apache=%d\n%s", mitIdx, apacheIdx, got)
	}
	if stdlibIdx >= mitIdx || mitIdx >= apacheIdx {
		t.Errorf("section order wrong: stdlib=%d mit=%d apache=%d", stdlibIdx, mitIdx, apacheIdx)
	}

	// Module lines carry versions; the NOTICE is reproduced indented
	// under its group.
	for _, want := range []string{
		"  - example.com/aaa/tool v2.0.0",
		"  - example.com/zzz/sdk v1.2.3",
		"  - example.com/zzz/sdk/sub v0.9.0",
		"NOTICE (example.com/zzz/sdk):",
		"    Example Product",
		"    Copyright 2015 Example, Inc.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}

	// The shared Apache text must appear exactly once.
	if n := strings.Count(got, "Apache License\n"); n != 1 {
		t.Errorf("shared license text appears %d times, want 1", n)
	}

	// Determinism: a second run over the same inputs is byte-identical.
	again, err := buildNotices("Copyright 2009 The Go Authors.\n(stdlib text)\n", mods)
	if err != nil {
		t.Fatalf("buildNotices (second run): %v", err)
	}
	if got != again {
		t.Error("buildNotices is not deterministic across runs")
	}
}

func TestBuildNotices_CRLFNormalization(t *testing.T) {
	t.Parallel()
	// The same license text with CRLF and LF endings must land in one
	// group, and no carriage returns may leak into the document.
	crlfDir := writeModuleDir(t, map[string]string{
		"LICENSE": strings.ReplaceAll(fakeBSD, "\n", "\r\n"),
	})
	lfDir := writeModuleDir(t, map[string]string{"LICENSE": fakeBSD})

	got, err := buildNotices("stdlib", []moduleInfo{
		{Path: "example.com/crlf", Version: "v1.0.0", Dir: crlfDir},
		{Path: "example.com/lf", Version: "v1.0.0", Dir: lfDir},
	})
	if err != nil {
		t.Fatalf("buildNotices: %v", err)
	}
	if strings.Contains(got, "\r") {
		t.Error("output contains carriage returns")
	}
	if !strings.Contains(got, "example.com/crlf and related modules (2 modules) — BSD 3-Clause License") {
		t.Errorf("CRLF and LF variants of the same text were not grouped:\n%s", got)
	}
}

func TestBuildNotices_UnknownLicenseFails(t *testing.T) {
	t.Parallel()
	dir := writeModuleDir(t, map[string]string{"LICENSE": "Example Public License v42\n"})
	_, err := buildNotices("stdlib", []moduleInfo{{Path: "example.com/x", Version: "v1.0.0", Dir: dir}})
	if err == nil || !strings.Contains(err.Error(), "example.com/x") {
		t.Fatalf("want classification error naming the module, got %v", err)
	}
}

func TestBuildNotices_MissingLicenseFails(t *testing.T) {
	t.Parallel()
	dir := writeModuleDir(t, map[string]string{"README.md": "no license here\n"})
	_, err := buildNotices("stdlib", []moduleInfo{{Path: "example.com/x", Version: "v1.0.0", Dir: dir}})
	if err == nil || !strings.Contains(err.Error(), "example.com/x") {
		t.Fatalf("want missing-license error naming the module, got %v", err)
	}
}

func TestFindFirstFile_PrefixPriorityAndCase(t *testing.T) {
	t.Parallel()
	dir := writeModuleDir(t, map[string]string{
		"COPYING":     "copying text",
		"license.md":  "lowercase license",
		"LICENSE.txt": "uppercase license",
	})
	got, err := findFirstFile(dir, "LICENSE", "COPYING")
	if err != nil {
		t.Fatalf("findFirstFile: %v", err)
	}
	// LICENSE-prefixed files win over COPYING; among them the
	// lexicographically first name is chosen deterministically.
	if base := filepath.Base(got); base != "LICENSE.txt" {
		t.Errorf("findFirstFile = %s, want LICENSE.txt", base)
	}
}
