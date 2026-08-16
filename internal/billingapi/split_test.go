package billingapi_test

import "testing"

func TestSplitDatabaseName(t *testing.T) {
	cases := []struct {
		dsn      string
		wantBase string
		wantName string
		wantQ    string
		wantOk   bool
	}{
		{"postgres://u:p@h:5432/otela_test", "postgres://u:p@h:5432/", "otela_test", "", true},
		// A query string must not make the DSN unparseable: the name is
		// extracted without it and the query preserved for derived DSNs.
		{"postgres://u:p@h:5432/otela_test?sslmode=disable", "postgres://u:p@h:5432/", "otela_test", "?sslmode=disable", true},
		{"postgres://u:p@h:5432/otela_test?sslmode=disable&application_name=x", "postgres://u:p@h:5432/", "otela_test", "?sslmode=disable&application_name=x", true},
		{"postgres://u:p@h/otela", "postgres://u:p@h/", "otela", "", true},
		// The final slash delimits the database name.
		{"postgres://u:p@h:5432/otela/extra", "postgres://u:p@h:5432/otela/", "extra", "", true},
		{"not-a-dsn", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.dsn, func(t *testing.T) {
			base, name, q, ok := splitDatabaseName(tc.dsn)
			if ok != tc.wantOk || base != tc.wantBase || name != tc.wantName || q != tc.wantQ {
				t.Fatalf("got (%q,%q,%q,%v), want (%q,%q,%q,%v)",
					base, name, q, ok, tc.wantBase, tc.wantName, tc.wantQ, tc.wantOk)
			}
		})
	}
}
