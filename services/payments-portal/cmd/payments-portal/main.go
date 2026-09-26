// Command payments-portal serves the web portals over payments-svc: a
// client's treasury, a bank's operations staff and operator operations. Every
// page comes from payments-svc's public API, called through the generated
// Kratos HTTP clients as the signed-in person.
//
// Sign-in is a demo user picker (the identities payments-svc's demo config
// accepts), so the portal is a demo build only; a production portal would
// sign people in with OpenID Connect.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"omnibus-deposit-network/services/payments-portal/portal"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8091", "listen address")
	api := flag.String("api", "http://127.0.0.1:8090", "payments-svc base URL")
	users := flag.String("users", "../payments-svc/configs/config.yaml", "payments-svc demo config listing auth.static_users")
	flag.Parse()
	if !demoBuild {
		log.Fatal("payments-portal signs in with a demo user picker; build it with -tags demo")
	}
	us, err := loadUsers(*users)
	if err != nil {
		log.Fatal(err)
	}
	p, cleanup, err := portal.New(*api, us)
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()
	srv := &http.Server{Addr: *addr, Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("payments-portal on http://%s (API %s)", *addr, *api)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadUsers(path string) ([]portal.User, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c struct {
		Auth struct {
			StaticUsers []struct {
				Token        string `yaml:"token"`
				Name         string `yaml:"name"`
				Role         string `yaml:"role"`
				Bank         string `yaml:"bank"`
				Organization string `yaml:"organization"`
			} `yaml:"static_users"`
		} `yaml:"auth"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	var out []portal.User
	for _, u := range c.Auth.StaticUsers {
		out = append(out, portal.User{Token: u.Token, Name: u.Name, Role: u.Role, Bank: u.Bank, Organization: u.Organization})
	}
	return out, nil
}
