package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nkoteb/nimbus/internal/secrets"
)

func (e *Env) keyPath() string {
	return filepath.Join(e.Paths.Config, "age.key")
}

func runSecrets(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nimbus secrets <keygen|recipient|encrypt|decrypt> [args]")
	}
	switch args[0] {
	case "keygen":
		return runSecretsKeygen(ctx, env, args[1:])
	case "recipient":
		return runSecretsRecipient(ctx, env, args[1:])
	case "encrypt":
		return runSecretsEncrypt(ctx, env, args[1:])
	case "decrypt":
		return runSecretsDecrypt(ctx, env, args[1:])
	default:
		return fmt.Errorf("unknown secrets subcommand %q", args[0])
	}
}

func runSecretsKeygen(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secrets keygen", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	force := fs.Bool("force", false, "overwrite an existing key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := env.keyPath()
	if _, err := os.Stat(path); err == nil && !*force {
		// Overwriting silently would make every existing secret undecryptable.
		return fmt.Errorf("key already exists at %s (use --force to replace it, "+
			"but everything encrypted to the old key becomes unreadable)", path)
	}

	if err := env.Paths.EnsureDirs(); err != nil {
		return err
	}

	kp, err := secrets.Generate()
	if err != nil {
		return err
	}
	if err := kp.Save(path); err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "key written to %s\n", path)
	fmt.Fprintf(env.Out, "public recipient: %s\n", kp.Recipient())
	return nil
}

func runSecretsRecipient(_ context.Context, env *Env, args []string) error {
	kp, err := secrets.LoadKey(env.keyPath())
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Out, kp.Recipient())
	return nil
}

func runSecretsEncrypt(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secrets encrypt", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	var recipients multiFlag
	fs.Var(&recipients, "to", "recipient public key (repeatable; defaults to this device)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if len(recipients) == 0 {
		kp, err := secrets.LoadKey(env.keyPath())
		if err != nil {
			return err
		}
		recipients = append(recipients, kp.Recipient())
	}

	plaintext, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	out, err := secrets.Encrypt(plaintext, recipients)
	if err != nil {
		return err
	}
	_, err = env.Out.Write(out)
	return err
}

func runSecretsDecrypt(_ context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("secrets decrypt", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	kp, err := secrets.LoadKey(env.keyPath())
	if err != nil {
		return err
	}

	ciphertext, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	out, err := kp.Decrypt(ciphertext)
	if err != nil {
		return err
	}
	_, err = env.Out.Write(out)
	return err
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint(*m) }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
