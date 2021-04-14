package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang/glog"
	ptpv1 "github.com/openshift/ptp-operator/api/v1"
	"github.com/openshift/ptp-operator/pkg/names"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ProcessManager manages a set of ptpProcess
// which could be ptp4l, phc2sys or timemaster.
// Processes in ProcessManager will be started
// or stopped simultaneously.
type ProcessManager struct {
	process []*ptpProcess
}

type ptpProcess struct {
	name            string
	iface           string
	ptp4lSocketPath string
	ptp4lConfigPath string
	exitCh          chan bool
	cmd             *exec.Cmd
}

type DaemonReconciler struct {
	client.Client
	Log            logr.Logger
	Scheme         *runtime.Scheme
	NodeName       string
	ProfileDir     string
	PtpUpdate      *LinuxPTPConfUpdate
	ProcessManager *ProcessManager
}

func (r *DaemonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcile.Result, error) {
	reqLogger := r.Log.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)
	reqLogger.Info("Reconciling ConfigMap")

	err := r.applyNodePTPProfiles()
	if err != nil {
		glog.Errorf("linuxPTP apply node profile failed: %v", err)
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *DaemonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return r.shouldReconcileObject(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return r.shouldReconcileObject(e.ObjectNew)
			},
		}).
		Complete(r)
}

func (r *DaemonReconciler) StopAllProcesses() {
	for _, p := range r.ProcessManager.process {
		if p != nil {
			glog.Infof("stopping process.... %+v", p)
			cmdStop(p)
			p = nil
		}
	}
}

func printWhenNotNil(p *string, description string) {
	if p != nil {
		glog.Info(description, ": ", *p)
	}
}

func (r *DaemonReconciler) applyNodePTPProfiles() error {
	glog.Infof("in applyNodePTPProfiles")

	r.StopAllProcesses()

	// All process should have been stopped,
	// clear process in process manager.
	// Assigning processManager.process to nil releases
	// the underlying slice to the garbage
	// collector (assuming there are no other
	// references).
	r.ProcessManager.process = nil

	// TODO:
	// compare nodeProfile with previous config,
	// only apply when nodeProfile changes

	glog.Infof("updating NodePTPProfiles to:")
	runID := 0
	for _, profile := range r.PtpUpdate.NodeProfiles {
		err := r.applyNodePtpProfile(runID, &profile)
		if err != nil {
			return err
		}
		runID++
	}

	// Start all the process
	for _, p := range r.ProcessManager.process {
		if p != nil {
			time.Sleep(1 * time.Second)
			go cmdRun(p)
		}
	}
	return nil
}

func (r *DaemonReconciler) applyNodePtpProfile(runID int, nodeProfile *ptpv1.PtpProfile) error {
	// This add the flags needed for monitor
	addFlagsForMonitor(nodeProfile)

	socketPath := fmt.Sprintf("/var/run/ptp4l.%d.socket", runID)
	// This will create the configuration needed to run the ptp4l and phc2sys
	r.addProfileConfig(socketPath, nodeProfile)

	glog.Infof("------------------------------------")
	printWhenNotNil(nodeProfile.Name, "Profile Name")
	printWhenNotNil(nodeProfile.Interface, "Interface")
	printWhenNotNil(nodeProfile.Ptp4lOpts, "Ptp4lOpts")
	printWhenNotNil(nodeProfile.Ptp4lConf, "Ptp4lConf")
	printWhenNotNil(nodeProfile.Phc2sysOpts, "Phc2sysOpts")
	glog.Infof("------------------------------------")

	if nodeProfile.Phc2sysOpts != nil {
		r.ProcessManager.process = append(r.ProcessManager.process, &ptpProcess{
			name:   "phc2sys",
			iface:  *nodeProfile.Interface,
			exitCh: make(chan bool),
			cmd:    phc2sysCreateCmd(nodeProfile)})
	} else {
		glog.Infof("applyNodePtpProfile: not starting phc2sys, phc2sysOpts is empty")
	}

	if nodeProfile.Ptp4lOpts != nil && nodeProfile.Interface != nil {
		configPath := fmt.Sprintf("/var/run/ptp4l.%d.config", runID)
		err := ioutil.WriteFile(configPath, []byte(*nodeProfile.Ptp4lConf), 0644)
		if err != nil {
			return fmt.Errorf("failed to write the configuration file named %s: %v", configPath, err)
		}

		r.ProcessManager.process = append(r.ProcessManager.process, &ptpProcess{
			name:            "ptp4l",
			iface:           *nodeProfile.Interface,
			ptp4lConfigPath: configPath,
			ptp4lSocketPath: socketPath,
			exitCh:          make(chan bool),
			cmd:             ptp4lCreateCmd(nodeProfile, configPath)})
	} else {
		glog.Infof("applyNodePtpProfile: not starting ptp4l, ptp4lOpts or interface is empty")
	}

	return nil
}

func (r *DaemonReconciler) addProfileConfig(socketPath string, nodeProfile *ptpv1.PtpProfile) {
	// TODO: later implement a merge capability
	if nodeProfile.Ptp4lConf == nil || *nodeProfile.Ptp4lConf == "" {
		// We need to copy this to another variable because is a pointer
		config := string(r.PtpUpdate.defaultPTP4lConfig)
		nodeProfile.Ptp4lConf = &config
	}

	config := fmt.Sprintf("%s\nuds_address %s\nmessage_tag [%s]",
		*nodeProfile.Ptp4lConf,
		socketPath,
		*nodeProfile.Interface)
	nodeProfile.Ptp4lConf = &config

	commandLine := fmt.Sprintf("%s -z %s -t [%s]",
		*nodeProfile.Phc2sysOpts,
		socketPath,
		*nodeProfile.Interface)
	nodeProfile.Phc2sysOpts = &commandLine
}

// phc2sysCreateCmd generate phc2sys command
func phc2sysCreateCmd(nodeProfile *ptpv1.PtpProfile) *exec.Cmd {
	cmdLine := fmt.Sprintf("/usr/sbin/phc2sys %s", *nodeProfile.Phc2sysOpts)
	args := strings.Split(cmdLine, " ")
	return exec.Command(args[0], args[1:]...)
}

// ptp4lCreateCmd generate ptp4l command
func ptp4lCreateCmd(nodeProfile *ptpv1.PtpProfile, confFilePath string) *exec.Cmd {
	cmdLine := fmt.Sprintf("/usr/sbin/ptp4l -f %s -i %s %s",
		confFilePath,
		*nodeProfile.Interface,
		*nodeProfile.Ptp4lOpts)

	args := strings.Split(cmdLine, " ")
	return exec.Command(args[0], args[1:]...)
}

// cmdRun runs given ptpProcess and wait for errors
func cmdRun(p *ptpProcess) {
	glog.Infof("Starting %s...", p.name)
	glog.Infof("%s cmd: %+v", p.name, p.cmd)

	defer func() {
		p.exitCh <- true
	}()

	//
	// don't discard process stderr output
	//
	p.cmd.Stderr = os.Stderr
	cmdReader, err := p.cmd.StdoutPipe()
	if err != nil {
		glog.Errorf("cmdRun() error creating StdoutPipe for %s: %v", p.name, err)
		return
	}

	done := make(chan struct{})

	scanner := bufio.NewScanner(cmdReader)
	go func() {
		for scanner.Scan() {
			output := scanner.Text()
			fmt.Printf("%s\n", output)
			extractMetrics(p.name, p.iface, output)
		}
		done <- struct{}{}
	}()

	err = p.cmd.Start()
	if err != nil {
		glog.Errorf("cmdRun() error starting %s: %v", p.name, err)
		return
	}

	<-done

	err = p.cmd.Wait()
	if err != nil {
		glog.Errorf("cmdRun() error waiting for %s: %v", p.name, err)
		return
	}
}

// cmdStop stops ptpProcess launched by cmdRun
func cmdStop(p *ptpProcess) {
	glog.Infof("Stopping %s...", p.name)
	if p.cmd == nil {
		return
	}

	if p.cmd.Process != nil {
		glog.Infof("Sending TERM to PID: %d", p.cmd.Process.Pid)
		p.cmd.Process.Signal(syscall.SIGTERM)
	}

	if p.ptp4lSocketPath != "" {
		err := os.Remove(p.ptp4lSocketPath)
		if err != nil {
			glog.Errorf("failed to remove ptp4l socket path %s: %v", p.ptp4lSocketPath, err)
		}
	}

	if p.ptp4lConfigPath != "" {
		err := os.Remove(p.ptp4lConfigPath)
		if err != nil {
			glog.Errorf("failed to remove ptp4l config path %s: %v", p.ptp4lConfigPath, err)
		}
	}

	<-p.exitCh
	glog.Infof("Process %d terminated", p.cmd.Process.Pid)
}

func (r *DaemonReconciler) shouldReconcileObject(o client.Object) bool {
	cm, ok := o.(*corev1.ConfigMap)
	if !ok {
		return false
	}

	if cm.Name != names.DefaultPTPConfigMapName {
		return false
	}

	nodeProfile := filepath.Join(r.ProfileDir, r.NodeName)
	if _, err := os.Stat(nodeProfile); err != nil {
		if os.IsNotExist(err) {
			glog.Infof("ptp profile doesn't exist for node: %v", r.NodeName)
			return false
		} else {
			glog.Errorf("error stating node profile %v: %v", r.NodeName, err)
			return false
		}
	}

	nodeProfilesJson, err := ioutil.ReadFile(nodeProfile)

	if err != nil {
		glog.Errorf("error reading node profile: %v", nodeProfile)
		return false
	}

	err = r.PtpUpdate.UpdateConfig(nodeProfilesJson)
	if err != nil {
		glog.Errorf("error updating the node configuration using the profiles loaded: %v", err)
	}

	return true
}
