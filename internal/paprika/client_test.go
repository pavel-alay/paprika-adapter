package paprika_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/paprikatest"
)

var day = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func newClient(t *testing.T, fake *paprikatest.Server, mutate ...func(*paprika.Options)) *paprika.Client {
	t.Helper()
	opts := paprika.Options{BaseURL: fake.URL, Email: paprikatest.Email, Password: paprikatest.Password, RetryWait: time.Millisecond, HTTPClient: fake.Client()}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := paprika.New(opts)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func newFake(t *testing.T) *paprikatest.Server {
	t.Helper()
	fake := paprikatest.New(day)
	t.Cleanup(fake.Close)
	return fake
}

func wantKind(t *testing.T, err error, kind paprika.Kind) {
	t.Helper()
	if got := paprika.KindOf(err); got != kind {
		t.Fatalf("error kind = %q (%v), want %q", got, err, kind)
	}
}

func TestLoginOnceThenReuseToken(t *testing.T) {
	fake := newFake(t)
	c := newClient(t, fake)
	for range 3 {
		rows, err := c.List(context.Background(), paprika.Meals)
		if err != nil {
			t.Fatalf("list meals: %v", err)
		}
		if len(rows) != 5 {
			t.Fatalf("meals = %d, want 5", len(rows))
		}
	}
	if n := fake.Hits("POST /api/v1/account/login/"); n != 1 {
		t.Fatalf("logins = %d, want 1", n)
	}
}

func TestTokenFileIsPrivateAndReusedAcrossClients(t *testing.T) {
	fake := newFake(t)
	path := filepath.Join(t.TempDir(), "token")
	withFile := func(o *paprika.Options) { o.TokenFile = path }
	if _, err := newClient(t, fake, withFile).Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := newClient(t, fake, withFile).Status(context.Background()); err != nil {
		t.Fatalf("status with cached token: %v", err)
	}
	if n := fake.Hits("POST /api/v1/account/login/"); n != 1 {
		t.Fatalf("logins = %d, want 1 (second client must reuse the token file)", n)
	}
}

func TestExpiredTokenTriggersOneRelogin(t *testing.T) {
	fake := newFake(t)
	c := newClient(t, fake)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	fake.RevokeToken()
	if _, err := c.List(context.Background(), paprika.Pantry); err != nil {
		t.Fatalf("list after token revocation: %v", err)
	}
	if n := fake.Hits("POST /api/v1/account/login/"); n != 2 {
		t.Fatalf("logins = %d, want 2", n)
	}
}

func TestBadCredentialsAreAuthFailureAndNotHammered(t *testing.T) {
	fake := newFake(t)
	c := newClient(t, fake, func(o *paprika.Options) { o.Password = "wrong" })
	for range 3 {
		_, err := c.Status(context.Background())
		wantKind(t, err, paprika.KindAuth)
	}
	if n := fake.Hits("POST /api/v1/account/login/"); n != 1 {
		t.Fatalf("logins = %d, want 1 (rejected credentials must not be retried immediately)", n)
	}
}

func TestErrorSentAsHTTP200IsAnError(t *testing.T) {
	fake := newFake(t)
	_, err := newClient(t, fake).Recipe(context.Background(), "NO-SUCH-RECIPE")
	wantKind(t, err, paprika.KindNotFound)
}

func TestReadsRetryTransientFailures(t *testing.T) {
	fake := newFake(t)
	fake.FailNext("GET /api/v2/sync/meals/", 503, 429)
	if _, err := newClient(t, fake).List(context.Background(), paprika.Meals); err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := fake.Hits("GET /api/v2/sync/meals/"); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestReadsGiveUpAsUnavailable(t *testing.T) {
	fake := newFake(t)
	fake.SetDown(true)
	_, err := newClient(t, fake).Status(context.Background())
	wantKind(t, err, paprika.KindUnavailable)
}

func TestWritesAreSentOnce(t *testing.T) {
	fake := newFake(t)
	fake.FailNext("POST /api/v2/sync/groceries/", 503)
	err := newClient(t, fake).AddGroceryItems(context.Background(), []paprika.NewGroceryItem{{UID: paprika.NewUID(), ListUID: "L", Name: "x"}})
	wantKind(t, err, paprika.KindUnavailable)
	if n := fake.Hits("POST /api/v2/sync/groceries/"); n != 1 {
		t.Fatalf("POST attempts = %d, want 1", n)
	}
}

func TestWriteUsesGzippedMultipartArray(t *testing.T) {
	fake := newFake(t)
	uid := paprika.NewUID()
	aisle := "AISLE"
	err := newClient(t, fake).AddGroceryItems(context.Background(), []paprika.NewGroceryItem{{
		UID: uid, ListUID: "LIST", Name: "2 limes", Ingredient: "limes", Quantity: "2", AisleUID: &aisle, OrderFlag: 7,
	}})
	if err != nil {
		t.Fatalf("add: %v", err) // the fake only accepts multipart "data" holding a gzipped JSON array
	}
	for _, r := range fake.Rows("groceries") {
		if r["uid"] != uid {
			continue
		}
		want := map[string]any{"list_uid": "LIST", "name": "2 limes", "ingredient": "limes", "quantity": "2",
			"aisle_uid": "AISLE", "order_flag": float64(7), "purchased": false, "separate": false, "recipe": nil, "instruction": ""}
		for k, v := range want {
			if r[k] != v {
				t.Errorf("stored %s = %#v, want %#v", k, r[k], v)
			}
		}
		return
	}
	t.Fatal("item not stored by the server")
}

func TestMalformedUIDIsRefusedLocally(t *testing.T) {
	fake := newFake(t)
	c := newClient(t, fake)
	for _, uid := range []string{"", "abc", strings.ToLower(paprika.NewUID()), "recipe_" + paprika.NewUID()} {
		wantKind(t, c.AddMeal(context.Background(), paprika.NewMeal{UID: uid, Name: "x"}), paprika.KindInvalid)
	}
	if n := fake.Hits("POST "); n != 0 {
		t.Fatalf("requests sent = %d, want 0", n)
	}
}

func TestUnexpectedResponseShapeIsProtocolError(t *testing.T) {
	for name, body := range map[string]string{
		"not json":           "<html>maintenance</html>",
		"no result or error": `{"data": []}`,
		"rows without uid":   `{"result": [{"name": "x"}]}`,
		"result not a list":  `{"result": {"uid": "x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			hc := paprikatest.HandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/login/") {
					_, _ = w.Write([]byte(`{"result":{"token":"t"}}`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			c, err := paprika.New(paprika.Options{BaseURL: "https://paprika.test", Email: "a", Password: "b", RetryWait: time.Millisecond, HTTPClient: hc})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.List(context.Background(), paprika.Meals)
			wantKind(t, err, paprika.KindProtocol)
		})
	}
}

func TestCredentialsOnlyTravelOverTLSOrLoopback(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://www.paprikaapp.com": true, "http://127.0.0.1:8080": true, "http://localhost:1": true,
		"http://www.paprikaapp.com": false, "http://10.0.0.5": false, "ftp://x": false, "nonsense": false,
	} {
		_, err := paprika.New(paprika.Options{BaseURL: url, Email: "a", Password: "b"})
		if (err == nil) != ok {
			t.Errorf("base URL %s: err = %v, want ok = %v", url, err, ok)
		}
	}
}

func TestSecretsNeverReachLogsOrErrors(t *testing.T) {
	fake := newFake(t)
	var logs bytes.Buffer
	c := newClient(t, fake, func(o *paprika.Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := context.Background()
	var errs []string
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	_, err := c.Status(ctx)
	collect(err)
	_, err = c.Recipe(ctx, "NOPE")
	collect(err)
	fake.RevokeToken()
	_, err = c.List(ctx, paprika.Meals)
	collect(err)
	fake.SetDown(true)
	_, err = c.List(ctx, paprika.Meals)
	collect(err)
	collect(c.AddMeal(ctx, paprika.NewMeal{UID: paprika.NewUID(), Name: "x"}))
	collect(c.Login(ctx))

	all := logs.String() + strings.Join(errs, "\n")
	if logs.Len() == 0 || len(errs) < 3 {
		t.Fatalf("test did not exercise logging and errors (logs=%d bytes, errors=%d)", logs.Len(), len(errs))
	}
	for _, secret := range []string{paprikatest.Password, "fixture-token", "Bearer"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked into logs or errors", secret)
		}
	}
}

func TestDetectDrift(t *testing.T) {
	rows := []paprika.Raw{
		{UID: "1", JSON: []byte(`{"uid":"1","name":"A","order_flag":1,"cookbook_uid":null,"brand_new":true}`)},
		{UID: "2", JSON: []byte(`{"uid":"2","name":"B","order_flag":2}`)},
	}
	d := paprika.DetectDrift(string(paprika.GroceryAisles), rows)
	if len(d.Unknown) != 2 || d.Unknown[0] != "brand_new" || d.Unknown[1] != "cookbook_uid" || len(d.Missing) != 0 {
		t.Fatalf("drift = %+v, want unknown=[brand_new cookbook_uid]", d)
	}
	// A field counts as present when any row carries it, even as null.
	d = paprika.DetectDrift(string(paprika.Categories), rows)
	if len(d.Missing) != 1 || d.Missing[0] != "parent_uid" || len(d.Unknown) != 1 || d.Unknown[0] != "brand_new" {
		t.Fatalf("drift = %+v, want missing=[parent_uid] unknown=[brand_new]", d)
	}
	if !paprika.DetectDrift(string(paprika.Categories), nil).Empty() {
		t.Fatal("no rows must mean no drift")
	}
}
