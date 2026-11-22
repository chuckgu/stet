package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
)

// An approved write runs on Stet's own connection, so two facts about Stet's
// account are worth a warning in enforce mode (spec 10): that it is a
// superuser, and that it is not a member of an agent role.
// poolAs is a pool of the account user, on stet_test.
func poolAs(t *testing.T, pg, user, password string) *pgxpool.Pool {
	t.Helper()
	pool, err := openPool(context.Background(), "postgres://"+user+":"+password+"@"+pg+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCheckWarnsWhenStetIsASuperuser(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	su := superuser(t, pg)
	table := freshTable(t, connect(t, pg, "stet_scram", "scram-pw"), 1)

	t.Run("a superuser", func(t *testing.T) {
		stet := createRole(t, su, "stet", "superuser login password 'stet-pw'")
		pool := poolAs(t, pg, stet, "stet-pw")
		warnings, err := check(ctx, pool, enforceConfig("stet_md5", table))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("warnings: %q", warnings)
		if _, ok := warningWith(warnings, fmt.Sprintf("Stet's account %q is a superuser", stet), "SET LOCAL ROLE"); !ok {
			t.Errorf("warnings %q, want one that Stet's account %s is a superuser", warnings, stet)
		}
		for _, w := range warnings {
			if _, ok := warningWith([]string{w}, "is not a member of agent role"); ok {
				t.Errorf("warning %q: a superuser is a member of every role", w)
			}
		}
	})

	t.Run("not a superuser", func(t *testing.T) {
		warnings, err := check(ctx, stetPool(t, pg), enforceConfig("stet_md5", table))
		if err != nil {
			t.Fatal(err)
		}
		if w, ok := warningWith(warnings, "is a superuser"); ok {
			t.Errorf("warning %q for stet_scram, which is no superuser", w)
		}
	})
}

func TestCheckWarnsWhenStetIsNotAMemberOfAnAgentRole(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	su := superuser(t, pg)
	pool := stetPool(t, pg)
	table := freshTable(t, connect(t, pg, "stet_scram", "scram-pw"), 1)
	const part = "is not a member of agent role"

	agent := createRole(t, su, "agent", "login")
	warnings, err := check(ctx, pool, enforceConfig(agent, table))
	if err != nil {
		t.Fatal(err)
	}
	fix := fmt.Sprintf(`GRANT "%s" TO "stet_scram"`, agent)
	if _, ok := warningWith(warnings, `"stet_scram"`, part, fmt.Sprintf("%q", agent), fix); !ok {
		t.Errorf("warnings %q, want one that stet_scram is not a member of %s, with %s", warnings, agent, fix)
	}

	mustExec(t, su, fmt.Sprintf("grant %s to stet_scram", agent))
	if warnings, err = check(ctx, pool, enforceConfig(agent, table)); err != nil {
		t.Fatal(err)
	} else if w, ok := warningWith(warnings, part); ok {
		t.Errorf("after the GRANT: warning %q", w)
	}

	// stet_trust is not granted to anything, so it warns.
	if warnings, err = check(ctx, pool, enforceConfig("stet_trust", table)); err != nil {
		t.Fatal(err)
	} else if _, ok := warningWith(warnings, part, `"stet_trust"`); !ok {
		t.Errorf("warnings %q, want one for stet_trust", warnings)
	}

	// A role that does not exist is warned of as missing, not as this.
	if warnings, err = check(ctx, pool, enforceConfig("stet_no_such_role", table)); err != nil {
		t.Fatal(err)
	} else if w, ok := warningWith(warnings, part); ok {
		t.Errorf("warning %q for a role that does not exist", w)
	}
}

// With custody, Stet's account is never an agent's, and the guide's path is
// shadow mode: an agent account Stet's account is not a member of has every
// multi-row write recorded unsupported, so shadow mode warns of it too, in
// enforce's words. A role that is not there is the start's login check's to
// warn of. Without custody, shadow mode stays silent about it
// (TestCheckInShadowSaysNothingOfApprovals).
func TestCheckWarnsOfMembershipInShadowWithCustody(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	su := superuser(t, pg)
	pool := stetPool(t, pg)
	table := freshTable(t, connect(t, pg, "stet_scram", "scram-pw"), 1)
	const part = "is not a member of agent role"
	agent := createRole(t, su, "agent", "login")
	shadow := func(custody bool, roles ...string) *config.Config {
		cfg := enforceConfig(roles[0], table)
		cfg.AgentRoles = roles
		cfg.Mode = config.ModeShadow
		if custody {
			cfg.Credentials = "agents.yaml"
		}
		return cfg
	}

	warnings, err := check(ctx, pool, shadow(true, agent))
	if err != nil {
		t.Fatal(err)
	}
	fix := fmt.Sprintf(`GRANT "%s" TO "stet_scram"`, agent)
	if _, ok := warningWith(warnings, `Stet's account "stet_scram"`, part, fmt.Sprintf("%q", agent), fix); !ok {
		t.Errorf("custody, shadow: warnings %q, want one that stet_scram is not a member of %s, with %s", warnings, agent, fix)
	}
	if warnings, err = check(ctx, pool, shadow(false, agent)); err != nil {
		t.Fatal(err)
	} else if w, ok := warningWith(warnings, part); ok {
		t.Errorf("no custody, shadow: warning %q", w)
	}

	mustExec(t, su, fmt.Sprintf("grant %s to stet_scram", agent))
	if warnings, err = check(ctx, pool, shadow(true, agent, "stet_no_such_role")); err != nil {
		t.Fatal(err)
	} else if w, ok := warningWith(warnings, part); ok {
		t.Errorf("custody, shadow, after the GRANT and with a role that is not there: warning %q", w)
	}
}

func TestCheckInShadowSaysNothingOfApprovals(t *testing.T) {
	pg := testPG(t)
	su := superuser(t, pg)
	table := freshTable(t, connect(t, pg, "stet_scram", "scram-pw"), 1)
	agent := createRole(t, su, "agent", "login")
	stet := createRole(t, su, "stet", "superuser login password 'stet-pw'")

	for name, p := range map[string]string{"stet_scram": "scram-pw", stet: "stet-pw"} {
		cfg := enforceConfig(agent, table)
		cfg.Mode = config.ModeShadow
		warnings, err := check(context.Background(), poolAs(t, pg, name, p), cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range []string{"is a superuser", "is not a member of agent role"} {
			if w, ok := warningWith(warnings, part); ok {
				t.Errorf("shadow, account %s: warning %q", name, w)
			}
		}
	}
}
