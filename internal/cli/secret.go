package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/starknet-innovation/troupe/internal/progress"
	"github.com/starknet-innovation/troupe/internal/registry"
	"github.com/starknet-innovation/troupe/internal/secret"
)

const secretHelp = `Usage:
  troupe secret set AGENT SERVICE
  troupe secret check AGENT

  set    asks for the secret without showing it, encrypts it with this machine's TPM and
         gives the agent's sandbox access to it. Use it to add or rotate a key. Asks for
         sudo once.
  check  checks every secret the agent needs: stored, decryptable and wired to its
         sandbox. Never shows a value.

A service is one of the agent's sbxenv.yaml secrets or its base kit's secrets.json entries.
A new service, unlike a rotated key, reaches a running sandbox only after sandboxd restarts,
which stops every VM on the host.
`

func secretCommand(e env, args []string) int {
	if helpRequested(args) {
		fmt.Fprint(e.out, secretHelp)
		return 0
	}
	if len(args) == 0 {
		return usageError(e, secretHelp, "secret needs set or check")
	}
	action := args[0]
	rest, code, ok := subcommand(e, "secret "+action, secretHelp, args[1:], nil)
	if !ok {
		return code
	}
	if !(action == "set" && len(rest) == 2 || action == "check" && len(rest) == 1) {
		return usageError(e, secretHelp, "secret %s: wrong arguments", action)
	}
	a, err := lifecycleAgent(e, "secret", rest[0], true)
	if err != nil {
		return fail(e, err)
	}
	store, err := agentStore(e, rest[0], a)
	if err != nil {
		return fail(e, err)
	}
	printer := a.Printer
	report := func(step string, err error) int {
		var f secret.Failure
		if errors.Is(err, secret.ErrInterrupted) {
			printer.Print(step, progress.Failed, "Interrupted; nothing changed")
			return 130
		}
		if errors.As(err, &f) {
			printer.Print(step, progress.Failed, f.Class)
			return 1
		}
		return a.Report(err)
	}

	if action == "check" {
		results, err := store.Check(e.ctx)
		if err != nil {
			return report("secret check", err)
		}
		failed := 0
		for _, r := range results {
			switch {
			case r.Problem == "" && r.Wired:
				printer.Print("secret "+r.Service, progress.OK, "Stored, decrypts, wired")
			case r.Problem == "":
				printer.Print("secret "+r.Service, progress.OK, "Stored, decrypts; the sandbox does not exist yet")
			default:
				printer.Print("secret "+r.Service, progress.Failed, r.Problem)
				failed++
			}
		}
		if failed > 0 {
			return 1
		}
		return 0
	}

	service := rest[1]
	// A published agent may take a key for a service its kit declares but it was created
	// without; the service joins its record only once the key is stored.
	added, err := kitService(e, rest[0], service, store)
	if err != nil {
		return fail(e, err)
	}
	wired, newService, err := store.Set(e.ctx, service)
	if err != nil {
		return report("secret "+service, err)
	}
	if added != "" {
		if err := addService(added, service); err != nil {
			return fail(e, err)
		}
	}
	switch {
	case !wired:
		printer.Print("secret "+service, progress.OK, "Stored; the sandbox does not exist yet and picks it up when it is created")
	case newService:
		printer.Print("secret "+service, progress.OK, "Stored and wired")
		printer.Print("secret "+service, progress.Action,
			"This service is new to the running sandbox: it is used only after sandboxd restarts, which stops every VM on the host")
	default:
		printer.Print("secret "+service, progress.OK, "Stored and wired; the running sandbox uses the new key on its next request")
	}
	return 0
}

// kitService adds to store a placeholder service the published agent's kit declares but the
// agent has no key for yet, and returns the agent's folder to record it in once the key is stored;
// "" when there is nothing to add.
func kitService(e env, name, service string, store *secret.Store) (string, error) {
	for _, s := range store.Services {
		if s.Name == service {
			return "", nil
		}
	}
	reg, _, err := e.registry()
	if err != nil {
		return "", err
	}
	base, err := reg.Base(name)
	if err != nil || !registry.IsPublished(base) {
		return "", err
	}
	declared, err := secret.KitServices(base)
	if err != nil {
		return "", err
	}
	for _, s := range declared {
		if s.Name == service && s.Delivery == "placeholder" {
			folder, err := reg.Folder(name)
			if err != nil {
				return "", err
			}
			store.Services = append(store.Services, secret.Service{Name: s.Name, Placeholder: true, Env: s.Env,
				Hosts: s.Hosts, Value: strings.ReplaceAll(s.Placeholder, "{agent}", name)})
			return folder, nil
		}
	}
	return "", nil
}
