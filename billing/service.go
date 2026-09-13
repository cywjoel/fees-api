package billing

import (
	"context"
	"fmt"
	"os"

	"encore.dev/rlog"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"fees-api/internal/billflow"
)

// defaultTemporalHostPort is the address of a local `temporal server start-dev`.
const defaultTemporalHostPort = "127.0.0.1:7233"

// dataConverter is named once and shared because bill creation starts its
// workflow through the raw service API and must encode that input itself. Two
// different converters would hand the workflow input it could not read.
var dataConverter = converter.GetDefaultDataConverter()

// Service holds the Temporal client the API endpoints use and the worker that
// executes bill workflows.
//
// This is the one seam where Encore and Temporal do not compose automatically:
// Temporal is not an Encore resource, so the worker is an ordinary long-lived
// goroutine that something has to own. The service struct is that owner.
//
//encore:service
type Service struct {
	temporal client.Client
	worker   worker.Worker
}

// temporalHostPort is overridable so the service can point at a dev server, a
// compose cluster, or Temporal Cloud without a code change.
func temporalHostPort() string {
	if hp := os.Getenv("TEMPORAL_HOSTPORT"); hp != "" {
		return hp
	}
	return defaultTemporalHostPort
}

func temporalNamespace() string {
	if ns := os.Getenv("TEMPORAL_NAMESPACE"); ns != "" {
		return ns
	}
	return client.DefaultNamespace
}

// initService dials Temporal and starts the bill worker.
//
// A failure here fails startup deliberately: a Fees API whose worker is not
// running would accept bills that never accrue, close, or invoice, in silence.
func initService() (*Service, error) {
	hostPort := temporalHostPort()
	namespace := temporalNamespace()

	// A lazy client defers the connection until first use, so startup does not
	// depend on Temporal being reachable at exactly that instant.
	c, err := client.NewLazyClient(client.Options{
		HostPort:      hostPort,
		Namespace:     namespace,
		Logger:        temporalLogger{},
		DataConverter: dataConverter,
	})
	if err != nil {
		return nil, fmt.Errorf("billing: creating Temporal client for %s: %w", hostPort, err)
	}

	w := worker.New(c, billflow.TaskQueue, worker.Options{})
	// Explicit name, the same constant the start request names, so renaming the Go
	// function cannot silently orphan running bills.
	w.RegisterWorkflowWithOptions(billflow.BillWorkflow,
		workflow.RegisterOptions{Name: billflow.WorkflowTypeName})

	// Likewise explicit: the workflow refers to activities by constant rather than
	// by function reference, which keeps its package free of any dependency on this
	// one.
	acts := &Activities{}
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
func (s *Service) Shutdown(force context.Context) {
	rlog.Info("stopping bill worker")
	if s.worker != nil {
		// Blocks until in-flight tasks are returned to the server, so a deploy does
		// not abandon work mid-task; anything unfinished is retried by whichever
		// worker picks it up next. A bill's workflow outlives any worker process.
		s.worker.Stop()
	}
	if s.temporal != nil {
		s.temporal.Close()
	}
	rlog.Info("bill worker stopped")
}

// temporalLogger routes the SDK's logs through Encore's structured logger.
type temporalLogger struct{}

func (temporalLogger) Debug(msg string, kv ...any) { rlog.Debug(msg, toStrings(kv)...) }
func (temporalLogger) Info(msg string, kv ...any)  { rlog.Info(msg, toStrings(kv)...) }
func (temporalLogger) Warn(msg string, kv ...any)  { rlog.Warn(msg, toStrings(kv)...) }
func (temporalLogger) Error(msg string, kv ...any) { rlog.Error(msg, toStrings(kv)...) }

// toStrings normalises the SDK's alternating key/value pairs for rlog, whose keys
// must be strings.
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
