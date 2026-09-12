package billing

import (
	"context"
	"fmt"
	"os"

	"encore.dev/rlog"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"fees-api/internal/billflow"
)

// defaultTemporalHostPort is the address of a local `temporal server start-dev`.
const defaultTemporalHostPort = "127.0.0.1:7233"

// Service holds the Temporal client the API endpoints use and the worker that
// executes bill workflows.
//
// This is the one seam where Encore and Temporal do not compose automatically.
// Encore provisions its own infrastructure - databases, queues, cron - from
// declarations in code, but Temporal is not one of its resources, so the worker
// is an ordinary long-lived goroutine that something has to own. An Encore
// service struct is that owner: initService starts the worker when the service
// starts, and Shutdown stops it when the service stops.
//
//encore:service
type Service struct {
	temporal client.Client
	worker   worker.Worker
}

// temporalHostPort returns the Temporal frontend address, overridable so the
// service can point at a dev server, a docker-compose cluster, or Temporal Cloud
// without a code change.
func temporalHostPort() string {
	if hp := os.Getenv("TEMPORAL_HOSTPORT"); hp != "" {
		return hp
	}
	return defaultTemporalHostPort
}

// temporalNamespace returns the namespace to run bills in.
func temporalNamespace() string {
	if ns := os.Getenv("TEMPORAL_NAMESPACE"); ns != "" {
		return ns
	}
	return client.DefaultNamespace
}

// initService dials Temporal and starts the bill worker.
//
// Encore calls this once when the service starts. A failure here fails startup,
// which is the behaviour we want: a Fees API whose worker is not running would
// accept bills that never accrue, close, or invoice, and would do so silently.
func initService() (*Service, error) {
	hostPort := temporalHostPort()
	namespace := temporalNamespace()

	// A lazy client defers the connection until first use, so startup does not
	// depend on Temporal being reachable at exactly that instant.
	c, err := client.NewLazyClient(client.Options{
		HostPort:  hostPort,
		Namespace: namespace,
		Logger:    temporalLogger{},
	})
	if err != nil {
		return nil, fmt.Errorf("billing: creating Temporal client for %s: %w", hostPort, err)
	}

	w := worker.New(c, billflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(billflow.BillWorkflow)

	acts := &Activities{}
	// Activities are registered under explicit names. The workflow refers to them
	// by those same constants rather than by Go function reference, which keeps
	// the workflow package free of any dependency on this one.
	w.RegisterActivityWithOptions(acts.PersistInvoice,
		activity.RegisterOptions{Name: billflow.ActivityPersistInvoice})
	w.RegisterActivityWithOptions(acts.EmitInvoice,
		activity.RegisterOptions{Name: billflow.ActivityEmitInvoice})
	w.RegisterActivityWithOptions(acts.FinalizeInvoice,
		activity.RegisterOptions{Name: billflow.ActivityFinalizeInvoice})

	if err := w.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("billing: starting Temporal worker on %q: %w", billflow.TaskQueue, err)
	}

	rlog.Info("bill worker started",
		"taskQueue", billflow.TaskQueue, "hostPort", hostPort, "namespace", namespace)

	return &Service{temporal: c, worker: w}, nil
}

// Shutdown stops the worker and closes the Temporal connection.
//
// Encore calls this on graceful shutdown. worker.Stop blocks until in-flight
// workflow and activity tasks have been returned to the server, so a deploy does
// not abandon work mid-task; anything not finished is simply retried by whichever
// worker picks it up next. That is the ordinary case, not an error path: a bill's
// workflow outlives any individual worker process by design.
func (s *Service) Shutdown(force context.Context) {
	rlog.Info("stopping bill worker")
	if s.worker != nil {
		s.worker.Stop()
	}
	if s.temporal != nil {
		s.temporal.Close()
	}
	rlog.Info("bill worker stopped")
}

// temporalLogger routes the Temporal SDK's logs through Encore's structured
// logger, so worker output appears in the same stream as everything else.
type temporalLogger struct{}

func (temporalLogger) Debug(msg string, kv ...any) { rlog.Debug(msg, toStrings(kv)...) }
func (temporalLogger) Info(msg string, kv ...any)  { rlog.Info(msg, toStrings(kv)...) }
func (temporalLogger) Warn(msg string, kv ...any)  { rlog.Warn(msg, toStrings(kv)...) }
func (temporalLogger) Error(msg string, kv ...any) { rlog.Error(msg, toStrings(kv)...) }

// toStrings normalises the SDK's alternating key/value pairs for rlog, whose
// keys must be strings.
func toStrings(kv []any) []any {
	out := make([]any, 0, len(kv))
	for i, v := range kv {
		if i%2 == 0 {
			out = append(out, fmt.Sprint(v))
			continue
		}
		out = append(out, v)
	}
	// An odd number of elements would leave a dangling key; drop it rather than
	// letting rlog reject the whole line.
	if len(out)%2 != 0 {
		out = out[:len(out)-1]
	}
	return out
}
