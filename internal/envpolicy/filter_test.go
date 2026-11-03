package envpolicy

import (
	"reflect"
	"testing"
)

func TestFilter(t *testing.T) {
	env := []string{"PATH=fixture", "HRN_TENANT_TOKEN=secret", "hrn_operator_token=secret", "HRN_DATABASE_URL=private", "HrN_Database_Url=private", "HRN_TOKEN_X=kept", "WITHOUT_EQUALS", "hRn_foo_ToKeN"}
	want := []string{"PATH=fixture", "HRN_TOKEN_X=kept", "WITHOUT_EQUALS"}
	if got := Filter(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("Filter() = %q, want %q", got, want)
	}
}
