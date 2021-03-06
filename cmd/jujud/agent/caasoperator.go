// Copyright 2017 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package agent

import (
	"runtime"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/gnuflag"
	"github.com/juju/utils/clock"
	"github.com/juju/utils/featureflag"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/juju/names.v2"
	"gopkg.in/juju/worker.v1"
	"gopkg.in/tomb.v1"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/cmd/jujud/agent/caasoperator"
	cmdutil "github.com/juju/juju/cmd/jujud/util"
	jujuversion "github.com/juju/juju/version"
	jworker "github.com/juju/juju/worker"
	"github.com/juju/juju/worker/dependency"
	"github.com/juju/juju/worker/introspection"
	"github.com/juju/juju/worker/logsender"
)

var (
	// Should be an explicit dependency, can't do it cleanly yet.
	// Exported for testing.
	CaasOperatorManifolds = caasoperator.Manifolds
)

// CaasOperatorAgent is a cmd.Command responsible for running a CAAS operator agent.
type CaasOperatorAgent struct {
	cmd.CommandBase
	tomb tomb.Tomb
	AgentConf
	ApplicationName string
	runner          *worker.Runner
	bufferedLogger  *logsender.BufferedLogWriter
	setupLogging    func(agent.Config) error
	ctx             *cmd.Context

	// Used to signal that the upgrade worker will not
	// reboot the agent on startup because there are no
	// longer any immediately pending agent upgrades.
	// Channel used as a selectable bool (closed means true).
	initialUpgradeCheckComplete chan struct{}

	prometheusRegistry *prometheus.Registry
}

// NewCaasOperatorAgent creates a new CAASOperatorAgent instance properly initialized.
func NewCaasOperatorAgent(ctx *cmd.Context, bufferedLogger *logsender.BufferedLogWriter) (*CaasOperatorAgent, error) {
	prometheusRegistry, err := newPrometheusRegistry()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &CaasOperatorAgent{
		AgentConf: NewAgentConf(""),
		ctx:       ctx,
		initialUpgradeCheckComplete: make(chan struct{}),
		bufferedLogger:              bufferedLogger,
		prometheusRegistry:          prometheusRegistry,
	}, nil
}

// Info implements Command.
func (op *CaasOperatorAgent) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "caasoperator",
		Purpose: "run a juju CAAS Operator",
	}
}

// SetFlags implements Command.
func (op *CaasOperatorAgent) SetFlags(f *gnuflag.FlagSet) {
	op.AgentConf.AddFlags(f)
	f.StringVar(&op.ApplicationName, "application-name", "", "name of the application")
}

// Init initializes the command for running.
func (op *CaasOperatorAgent) Init(args []string) error {
	if op.ApplicationName == "" {
		return cmdutil.RequiredError("application-name")
	}
	if !names.IsValidApplication(op.ApplicationName) {
		return errors.Errorf(`--application-name option expects "<application>" argument`)
	}
	if err := op.AgentConf.CheckArgs(args); err != nil {
		return err
	}
	op.runner = worker.NewRunner(worker.RunnerParams{
		IsFatal:       cmdutil.IsFatal,
		MoreImportant: cmdutil.MoreImportant,
		RestartDelay:  jworker.RestartDelay,
	})
	return nil
}

// Stop implements Worker.
func (op *CaasOperatorAgent) Stop() error {
	op.runner.Kill()
	return op.tomb.Wait()
}

// Run implements Command.
func (op *CaasOperatorAgent) Run(ctx *cmd.Context) error {
	defer op.tomb.Done()
	if err := op.ReadConfig(op.Tag().String()); err != nil {
		return err
	}
	logger.Infof("caas operator %v start (%s [%s])", op.Tag().String(), jujuversion.Current, runtime.Compiler)
	if flags := featureflag.String(); flags != "" {
		logger.Warningf("developer feature flags enabled: %s", flags)
	}

	op.runner.StartWorker("api", op.Workers)
	err := cmdutil.AgentDone(logger, op.runner.Wait())
	op.tomb.Kill(err)
	return err
}

// Workers returns a dependency.Engine running the operator's responsibilities.
func (op *CaasOperatorAgent) Workers() (worker.Worker, error) {
	manifolds := CaasOperatorManifolds(caasoperator.ManifoldsConfig{
		Agent:                op,
		Clock:                clock.WallClock,
		LogSource:            op.bufferedLogger.Logs(),
		PrometheusRegisterer: op.prometheusRegistry,
		LeadershipGuarantee:  30 * time.Second,
	})

	config := dependency.EngineConfig{
		IsFatal:     cmdutil.IsFatal,
		WorstError:  cmdutil.MoreImportantError,
		ErrorDelay:  3 * time.Second,
		BounceDelay: 10 * time.Millisecond,
	}
	engine, err := dependency.NewEngine(config)
	if err != nil {
		return nil, err
	}
	if err := dependency.Install(engine, manifolds); err != nil {
		if err := worker.Stop(engine); err != nil {
			logger.Errorf("while stopping engine with bad manifolds: %v", err)
		}
		return nil, err
	}
	if err := startIntrospection(introspectionConfig{
		Agent:              op,
		Engine:             engine,
		NewSocketName:      DefaultIntrospectionSocketName,
		PrometheusGatherer: op.prometheusRegistry,
		WorkerFunc:         introspection.NewWorker,
	}); err != nil {
		// If the introspection worker failed to start, we just log error
		// but continue. It is very unlikely to happen in the real world
		// as the only issue is connecting to the abstract domain socket
		// and the agent is controlled by by the OS to only have one.
		logger.Errorf("failed to start introspection worker: %v", err)
	}
	return engine, nil
}

// Tag implements Agent.
func (op *CaasOperatorAgent) Tag() names.Tag {
	return names.NewApplicationTag(op.ApplicationName)
}
