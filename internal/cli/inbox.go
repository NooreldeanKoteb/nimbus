package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/nkoteb/nimbus/internal/bus"
	"github.com/nkoteb/nimbus/internal/device"
)

func runInbox(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	all := fs.Bool("all", false, "include messages already acknowledged")
	ack := fs.Bool("ack", false, "acknowledge everything shown")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Reading a stale inbox is the same as having no inbox.
	autoRefresh(ctx, env)

	id, err := env.identity()
	if err != nil {
		return err
	}
	messages, err := bus.Inbox(env.Paths.Repo, id.ID, *all)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Fprintln(env.Out, "no messages")
		return nil
	}

	fleet := env.fleetLabels()
	for _, e := range messages {
		printMessage(env, e, fleet, true)
	}

	if !*ack {
		fmt.Fprintf(env.Out, "\n%d message(s) — `nimbus inbox --ack` to mark them read\n", len(messages))
		return nil
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	for _, e := range messages {
		if e.Delivered() {
			continue
		}
		if _, aerr := bus.Ack(env.Paths.Repo, id.ID, e.Message.ID, ""); aerr != nil {
			fmt.Fprintf(env.Out, "! could not acknowledge %s: %v\n", e.Message.ID, aerr)
		}
	}
	fmt.Fprintf(env.Out, "\nacknowledged %d message(s)\n", len(messages))
	autoSync(ctx, env, repo, id.ID, "nimbus: acknowledge messages")
	return nil
}

func runOutbox(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("outbox", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Delivery state lives in the recipient's receipts, so it only becomes
	// visible here after a pull.
	autoRefresh(ctx, env)

	id, err := env.identity()
	if err != nil {
		return err
	}
	messages, err := bus.Outbox(env.Paths.Repo, id.ID)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Fprintln(env.Out, "nothing sent from this device")
		return nil
	}

	fleet := env.fleetLabels()
	for _, e := range messages {
		printMessage(env, e, fleet, false)
	}
	return nil
}

func runAck(ctx context.Context, env *Env, args []string) error {
	msgID, args := takeArg(args)

	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	note := fs.String("note", "", "what you did about it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if msgID == "" {
		return errors.New("usage: nimbus ack <message-id>")
	}

	repo, err := env.stateRepo()
	if err != nil {
		return err
	}
	id, err := env.identity()
	if err != nil {
		return err
	}

	if _, err := bus.Ack(env.Paths.Repo, id.ID, msgID, *note); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "acknowledged %s\n", msgID)
	autoSync(ctx, env, repo, id.ID, "nimbus: acknowledge "+msgID)
	return nil
}

// printMessage renders one envelope. showFrom picks whether the interesting
// party is the sender (inbox) or the recipient (outbox).
func printMessage(env *Env, e bus.Envelope, fleet device.Fleet, showFrom bool) {
	party := fleet.Label(e.Message.To)
	direction := "to"
	if showFrom {
		party, direction = fleet.Label(e.Message.From), "from"
	}

	state := "unread"
	if e.Delivered() {
		state = "read " + e.Receipt.Received.Local().Format("Jan 2 15:04")
	}

	fmt.Fprintf(env.Out, "%s  %s %-16s %-9s [%s]\n",
		e.Message.Sent.Local().Format("Jan 2 15:04"), direction, party, e.Message.Kind, state)
	if e.Message.Task != "" {
		fmt.Fprintf(env.Out, "    task: %s  (nimbus resume %s)\n", e.Message.Task, e.Message.Task)
	}
	if e.Message.Text != "" {
		fmt.Fprintf(env.Out, "    %s\n", e.Message.Text)
	}
	if e.Receipt != nil && e.Receipt.Note != "" {
		fmt.Fprintf(env.Out, "    reply: %s\n", e.Receipt.Note)
	}
	fmt.Fprintf(env.Out, "    id: %s\n", e.Message.ID)
}
