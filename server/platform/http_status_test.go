package platform

import (
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestCreateProjectErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"tenant foreign key", &pgconn.PgError{Code: "23503", ConstraintName: "projects_tenant_id_fkey"}, http.StatusNotFound},
		{"unnamed foreign key", &pgconn.PgError{Code: "23503"}, http.StatusInternalServerError},
		{"wrapped tenant foreign key", errors.Join(errors.New("insert project"), &pgconn.PgError{Code: "23503", ConstraintName: "projects_tenant_id_fkey"}), http.StatusNotFound},
		{"other foreign key", &pgconn.PgError{Code: "23503", ConstraintName: "projects_other_id_fkey"}, http.StatusInternalServerError},
		{"other postgres error", &pgconn.PgError{Code: "23505", ConstraintName: "projects_tenant_id_fkey"}, http.StatusInternalServerError},
		{"storage error", errors.New("database unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := createProjectErrorStatus(tc.err); got != tc.want {
				t.Fatalf("status %d, want %d", got, tc.want)
			}
		})
	}
}

func TestScalingDemandErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"missing project", pgx.ErrNoRows, http.StatusNotFound},
		{"wrapped missing project", errors.Join(errors.New("scaling demand"), pgx.ErrNoRows), http.StatusNotFound},
		{"storage error", errors.New("database unavailable"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scalingDemandErrorStatus(tc.err); got != tc.want {
				t.Fatalf("status %d, want %d", got, tc.want)
			}
		})
	}
}
