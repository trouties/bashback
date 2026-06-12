package gitx

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in                 string
		major, minor       int
		meets, atLeast2_40 bool
	}{
		{"git version 2.34.1\n", 2, 34, true, false},
		{"git version 2.39.3 (Apple Git-145)\n", 2, 39, true, false},
		{"git version 2.40.0", 2, 40, true, true},
		{"git version 2.31.9", 2, 31, false, false},
		{"git version 3.0.0", 3, 0, true, true},
	}
	for _, c := range cases {
		v, err := parseVersion(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if v.Major != c.major || v.Minor != c.minor {
			t.Errorf("%q: got %d.%d, want %d.%d", c.in, v.Major, v.Minor, c.major, c.minor)
		}
		if v.MeetsMinimum() != c.meets {
			t.Errorf("%q: MeetsMinimum=%v, want %v", c.in, v.MeetsMinimum(), c.meets)
		}
		if v.AtLeast(2, 40) != c.atLeast2_40 {
			t.Errorf("%q: AtLeast(2,40)=%v, want %v", c.in, v.AtLeast(2, 40), c.atLeast2_40)
		}
	}
}

func TestParseVersionRejectsGarbage(t *testing.T) {
	if _, err := parseVersion("not git output"); err == nil {
		t.Fatal("expected error on garbage")
	}
}
