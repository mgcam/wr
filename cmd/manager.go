// Copyright © 2016-2018 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/VertebrateResequencing/wr/internal"
	"github.com/VertebrateResequencing/wr/jobqueue"
	jqs "github.com/VertebrateResequencing/wr/jobqueue/scheduler"
	"github.com/inconshreveable/log15"
	"github.com/kardianos/osext"
	"github.com/sb10/l15h"
	"github.com/sevlyar/go-daemon"
	"github.com/spf13/cobra"
)

// options for this cmd
var foreground bool
var scheduler string
var localUsername string
var backupPath string
var managerTimeoutSeconds int
var managerDebug bool

// managerCmd represents the manager command
var managerCmd = &cobra.Command{
	Use:   "manager",
	Short: "Workflow manager",
	Long: `The workflow management system.

The wr manager works in the background, doing all the work of ensuring your
commands get run successfully.

It maintains both a temporary queue of the commands you want to run, and a
permanent history of commands you've run in the past. As commands are added to
the queue, it makes sure to spawn sufficient 'wr runner' agents to get them all
run.

You'll need to start this daemon with the 'start' sub-command before you can
achieve anything useful with the other wr commands. If the background
process that is spawned when you run this dies, your workflows will become
stalled until you run the 'start' sub-command again.

If the manager fails to start or dies unexpectedly, you can check the logs which
are by default found in ~/.wr_[deployment]/log.

If using the OpenStack scheduler, note that you must be running on an OpenStack
server already. Be sure to set --local_username to your username outside of the
cloud, so that resources created will not conflict with anyone else in your
tenant (project) also running wr.
Instead you can use 'wr cloud deploy -p openstack' to create an OpenStack server
on which wr manager will be started in OpenStack mode for you. See 'wr cloud
deploy -h' for the details of which environment variables you need to use the
OpenStack scheduler.
If you want to start multiple managers up in different OpenStack networks that
you've created yourself, note that --local_username will need to be globally
unique, since it is used to name the private key that will be created in
OpenStack, and if a key with that name already exists, the manager will not be
able to create a new one (or get the existing one), and so will not function
fully.`,
}

// start sub-command starts the daemon
var managerStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start workflow management",
	Long: `Start the workflow manager, daemonizing it in to the background
(unless --foreground option is supplied).`,
	Run: func(cmd *cobra.Command, args []string) {
		// first we need our working directory to exist
		createWorkingDir()

		// check to see if the manager is already running (regardless of the
		// state of the pid file), giving us a meaningful error message in the
		// most obvious case of failure to start
		jq := connect(1 * time.Second)
		if jq != nil {
			die("wr manager on port %s is already running (pid %d)", config.ManagerPort, jq.ServerInfo.PID)
		}

		var postCreation []byte
		var extraArgs []string
		if postCreationScript != "" {
			var err error
			postCreation, err = ioutil.ReadFile(postCreationScript)
			if err != nil {
				die("--cloud_script %s could not be read: %s", postCreationScript, err)
			}

			// daemon runs from /, so we need to convert relative to absolute
			// path *** and then pretty hackily, re-specify the option by
			// repeating it on the end of os.Args, where the daemonization code
			// will pick it up
			pcsAbs, err := filepath.Abs(postCreationScript)
			if err != nil {
				die("--cloud_script %s could not be converted to an absolute path: %s", postCreationScript, err)
			}
			if pcsAbs != postCreationScript {
				extraArgs = append(extraArgs, "--cloud_script")
				extraArgs = append(extraArgs, pcsAbs)
			}
		}

		// now daemonize unless in foreground mode
		if foreground {
			syscall.Umask(config.ManagerUmask)
			startJQ(postCreation)
		} else {
			child, context := daemonize(config.ManagerPidFile, config.ManagerUmask, extraArgs...)
			if child != nil {
				// parent; wait a while for our child to bring up the manager
				// before exiting
				jq := connect(time.Duration(managerTimeoutSeconds) * time.Second)
				if jq == nil {
					die("wr manager failed to start on port %s after %ds", config.ManagerPort, managerTimeoutSeconds)
				}
				logStarted(jq.ServerInfo)
			} else {
				// daemonized child, that will run until signalled to stop
				defer func() {
					err := context.Release()
					if err != nil {
						warn("daemon release failed: %s", err)
					}
				}()
				startJQ(postCreation)
			}
		}
	},
}

// stop sub-command stops the daemon by sending it a term signal
var managerStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop workflow management",
	Long: `Immediately stop the workflow manager, saving its state.

Note that any runners that are currently running will die, along with any
commands they were running. It is more graceful to use 'drain' instead.`,
	Run: func(cmd *cobra.Command, args []string) {
		// the daemon could be running but be non-responsive, or it could have
		// exited but left the pid file in place; to best cover all
		// eventualities we check the pid file first, try and terminate its pid,
		// then confirm we can't connect
		pid, err := daemon.ReadPidFile(config.ManagerPidFile)
		var stopped bool
		if err == nil {
			stopped = stopdaemon(pid, "pid file "+config.ManagerPidFile)
		} else {
			// probably no pid file, we'll see if the daemon is up by trying to
			// connect
			jq := connect(1 * time.Second)
			if jq == nil {
				die("wr manager does not seem to be running on port %s", config.ManagerPort)
			}
		}

		var jq *jobqueue.Client
		if stopped {
			// we'll do a quick test to confirm the daemon is down
			jq = connect(1 * time.Second)
			if jq != nil {
				warn("according to the pid file %s, wr manager was running with pid %d, and I terminated that pid, but the manager is still up on port %s!", config.ManagerPidFile, pid, config.ManagerPort)
			} else {
				info("wr manager running on port %s was gracefully shut down", config.ManagerPort)
				return
			}
		} else {
			// we failed to SIGTERM the pid in the pid file, let's take some
			// time to confirm the daemon is really up
			jq = connect(5 * time.Second)
			if jq == nil {
				die("according to the pid file %s, wr manager for port %s was running with pid %d, but that process could not be terminated and the manager could not be connected to; most likely the pid file is wrong and the manager is not running - after confirming, delete the pid file before trying to start the manager again", config.ManagerPidFile, config.ManagerPort, pid)
			}
		}

		// we managed to connect to the daemon; try to stop it again using its
		// real pid; though it may actually be running on a remote host and we
		// managed to connect to it via ssh port forwarding; compare the server
		// ip to our own
		currentIP, err := jobqueue.CurrentIP("")
		if err != nil {
			warn("Could not get current IP: %s", err)
		}
		myAddr := currentIP + ":" + config.ManagerPort
		sAddr := jq.ServerInfo.Addr
		if myAddr == sAddr {
			err = jq.Disconnect()
			if err != nil {
				warn("Disconnecting from the server failed: %s", err)
			}
			stopped = stopdaemon(jq.ServerInfo.PID, "the manager itself")
		} else {
			// use the client command to stop it
			stopped = jq.ShutdownServer()

			// since I don't trust using a client connection to shut down the
			// server, double check I can no longer connect
			if stopped {
				jq = connect(1 * time.Second)
				if jq != nil {
					warn("I requested shut down of the remote manager at %s, but it's still up!", sAddr)
					stopped = false
				}
			}
		}

		if stopped {
			info("wr manager running at %s was gracefully shut down", sAddr)
		} else {
			die("I've tried everything; giving up trying to stop the manager at %s", sAddr)
		}
	},
}

// drain sub-command makes the server stop spawning new runners and stops it
// letting existing runners reserve jobs, and when there are no more runners
// running it will exit by itself
var managerDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Drain the workflow manager of running jobs and then stop",
	Long: `Wait for currently running jobs to finish and then gracefully stop the workflow manager, saving its state.

While draining you can continue to add new Jobs, but nothing new will start
running until the drain completes (or the manager is stopped) and the manager is
then started again.

It is safe to repeat this command to get an update on how long before the drain
completes.

NB: if using 'wr cloud deploy --deployment production', do not use drain without
also configuring an S3 location for your database backup, as otherwise any
changes to the database between calling drain and the manager finally shutting
down will be lost.`,
	Run: func(cmd *cobra.Command, args []string) {
		// first try and connect
		jq := connect(5 * time.Second)
		if jq == nil {
			die("could not connect to the manager on port %s, so could not initiate a drain; has it already been stopped?", config.ManagerPort)
		}

		// we managed to connect to the daemon; ask it to go in to drain mode
		numLeft, etc, err := jq.DrainServer()
		if err != nil {
			die("even though I was able to connect to the manager, it failed to enter drain mode: %s", err)
		}

		if numLeft == 0 {
			info("wr manager running on port %s is drained: there were no jobs still running, so the manger should stop right away.", config.ManagerPort)
		} else if numLeft == 1 {
			info("wr manager running on port %s is now draining; there is a job still running, and it should complete in less than %s", config.ManagerPort, etc)
		} else {
			info("wr manager running on port %s is now draining; there are %d jobs still running, and they should complete in less than %s", config.ManagerPort, numLeft, etc)
		}

		err = jq.Disconnect()
		if err != nil {
			warn("Disconnecting from the server failed: %s", err)
		}
	},
}

// status sub-command tells if the manger is up or down
var managerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the workflow manager",
	Long:  `Find out if the workflow manager is currently running or not.`,
	Run: func(cmd *cobra.Command, args []string) {
		// see if pid file suggests it is supposed to be running
		pid, err := daemon.ReadPidFile(config.ManagerPidFile)
		if err == nil {
			// confirm
			jq := connect(5 * time.Second)
			if jq != nil {
				reportLiveStatus(jq)
				return
			}

			die("wr manager on port %s is supposed to be running with pid %d, but is non-responsive", config.ManagerPort, pid)
		}

		// no pid file, so it's supposed to be down; confirm
		jq := connect(1 * time.Second)
		if jq == nil {
			fmt.Println("stopped")
		} else {
			reportLiveStatus(jq)
		}
	},
}

// backup sub-command does a database backup
var managerBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup wr's database",
	Long: `Manually backup wr's job database.

The manager automatically backs up its database to the configured location every
time there is a change.

You can use this command to create an additional backup to a different location.
Note that the manager must be running.

(When the manager is stopped, you can backup the database by simply copying it
somewhere.)`,
	Run: func(cmd *cobra.Command, args []string) {
		if backupPath == "" {
			die("--path is required")
		}
		timeout := time.Duration(timeoutint) * time.Second

		jq, err := jobqueue.Connect(addr, timeout)
		if err != nil {
			die("%s", err)
		}
		defer func() {
			err = jq.Disconnect()
			if err != nil {
				warn("Disconnecting from the server failed: %s", err)
			}
		}()

		err = jq.BackupDB(backupPath)
		if err != nil {
			die("%s", err)
		}
	},
}

// reportLiveStatus is used by the status command on a working connection to
// distinguish between the server being in a normal 'started' state or the
// 'drain' state.
func reportLiveStatus(jq *jobqueue.Client) {
	fmt.Println(jq.ServerInfo.Mode)
}

func init() {
	RootCmd.AddCommand(managerCmd)
	managerCmd.AddCommand(managerStartCmd)
	managerCmd.AddCommand(managerDrainCmd)
	managerCmd.AddCommand(managerStopCmd)
	managerCmd.AddCommand(managerStatusCmd)
	managerCmd.AddCommand(managerBackupCmd)

	// flags specific to these sub-commands
	defaultConfig := internal.DefaultConfig(appLogger)
	managerStartCmd.Flags().BoolVarP(&foreground, "foreground", "f", false, "do not daemonize")
	managerStartCmd.Flags().StringVarP(&scheduler, "scheduler", "s", defaultConfig.ManagerScheduler, "['local','lsf','openstack'] job scheduler")
	managerStartCmd.Flags().IntVarP(&managerTimeoutSeconds, "timeout", "t", 10, "how long to wait in seconds for the manager to start up")
	managerStartCmd.Flags().StringVarP(&osPrefix, "cloud_os", "o", defaultConfig.CloudOS, "for cloud schedulers, prefix name of the OS image your servers should use")
	managerStartCmd.Flags().StringVarP(&osUsername, "cloud_username", "u", defaultConfig.CloudUser, "for cloud schedulers, username needed to log in to the OS image specified by --cloud_os")
	managerStartCmd.Flags().StringVar(&localUsername, "local_username", realUsername(), "for cloud schedulers, your local username outside of the cloud")
	managerStartCmd.Flags().IntVarP(&osRAM, "cloud_ram", "r", defaultConfig.CloudRAM, "for cloud schedulers, ram (MB) needed by the OS image specified by --cloud_os")
	managerStartCmd.Flags().IntVarP(&osDisk, "cloud_disk", "d", defaultConfig.CloudDisk, "for cloud schedulers, minimum disk (GB) for servers")
	managerStartCmd.Flags().StringVarP(&flavorRegex, "cloud_flavor", "l", defaultConfig.CloudFlavor, "for cloud schedulers, a regular expression to limit server flavors that can be automatically picked")
	managerStartCmd.Flags().StringVarP(&postCreationScript, "cloud_script", "p", defaultConfig.CloudScript, "for cloud schedulers, path to a start-up script that will be run on each server created")
	managerStartCmd.Flags().IntVarP(&serverKeepAlive, "cloud_keepalive", "k", defaultConfig.CloudKeepAlive, "for cloud schedulers, how long in seconds to keep idle spawned servers alive for; 0 means forever")
	managerStartCmd.Flags().IntVarP(&maxServers, "cloud_servers", "m", defaultConfig.CloudServers, "for cloud schedulers, maximum number of additional servers to spawn; -1 means unlimited")
	managerStartCmd.Flags().StringVar(&cloudGatewayIP, "cloud_gateway_ip", defaultConfig.CloudGateway, "for cloud schedulers, gateway IP for the created subnet")
	managerStartCmd.Flags().StringVar(&cloudCIDR, "cloud_cidr", defaultConfig.CloudCIDR, "for cloud schedulers, CIDR of the created subnet")
	managerStartCmd.Flags().StringVar(&cloudDNS, "cloud_dns", defaultConfig.CloudDNS, "for cloud schedulers, comma separated DNS name server IPs to use in the created subnet")
	managerStartCmd.Flags().StringVar(&cloudConfigFiles, "cloud_config_files", defaultConfig.CloudConfigFiles, "for cloud schedulers, comma separated paths of config files to copy to spawned servers")
	managerStartCmd.Flags().BoolVar(&managerDebug, "debug", false, "include extra debugging information in the logs")

	managerBackupCmd.Flags().StringVarP(&backupPath, "path", "p", "", "backup file path")
}

func logStarted(s *jobqueue.ServerInfo) {
	info("wr manager started on %s, pid %d", sAddr(s), s.PID)
	info("wr's web interface can be reached at http://%s:%s", s.Host, s.WebPort)
}

func startJQ(postCreation []byte) {
	if runtime.NumCPU() == 1 {
		// we might lock up with only 1 proc if we mount
		runtime.GOMAXPROCS(2)
	} else {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	// change the app logger to log to both STDERR and our configured log file;
	// we also create a new logger for internal use by the server later
	serverLogger := log15.New()
	fh, err := log15.FileHandler(config.ManagerLogFile, log15.LogfmtFormat())
	if err != nil {
		warn("wr manager could not log to %s: %s", config.ManagerLogFile, err)
	} else {
		l15h.AddHandler(appLogger, fh)

		// have the server logger output to file, levelled with caller info
		logLevel := log15.LvlWarn
		if managerDebug {
			logLevel = log15.LvlDebug
		}
		serverLogger.SetHandler(log15.LvlFilterHandler(logLevel, l15h.CallerInfoHandler(fh)))
	}

	// we will spawn runners, which means we need to know the path to ourselves
	// in case we're not in the user's $PATH
	exe, err := osext.Executable()
	if err != nil {
		die("wr manager failed to start : %s\n", err)
	}

	var schedulerConfig interface{}
	serverCIDR := ""
	switch scheduler {
	case "local":
		schedulerConfig = &jqs.ConfigLocal{Shell: config.RunnerExecShell}
	case "lsf":
		schedulerConfig = &jqs.ConfigLSF{Deployment: config.Deployment, Shell: config.RunnerExecShell}
	case "openstack":
		mport, _ := strconv.Atoi(config.ManagerPort)
		schedulerConfig = &jqs.ConfigOpenStack{
			ResourceName:         cloudResourceName(localUsername),
			SavePath:             filepath.Join(config.ManagerDir, "cloud_resources.openstack"),
			ServerPorts:          []int{22, mport},
			OSPrefix:             osPrefix,
			OSUser:               osUsername,
			OSRAM:                osRAM,
			OSDisk:               osDisk,
			FlavorRegex:          flavorRegex,
			PostCreationScript:   postCreation,
			ConfigFiles:          cloudConfigFiles,
			ServerKeepTime:       time.Duration(serverKeepAlive) * time.Second,
			StateUpdateFrequency: 1 * time.Minute,
			MaxInstances:         maxServers,
			Shell:                config.RunnerExecShell,
			GatewayIP:            cloudGatewayIP,
			CIDR:                 cloudCIDR,
			DNSNameServers:       strings.Split(cloudDNS, ","),
		}
		serverCIDR = cloudCIDR
	}

	// start the jobqueue server
	server, msg, err := jobqueue.Serve(jobqueue.ServerConfig{
		AllowedUsers:    []string{localUsername},
		Port:            config.ManagerPort,
		WebPort:         config.ManagerWeb,
		SchedulerName:   scheduler,
		SchedulerConfig: schedulerConfig,
		RunnerCmd:       exe + " runner -s '%s' --deployment %s --server '%s' -r %d -m %d",
		DBFile:          config.ManagerDbFile,
		DBFileBackup:    config.ManagerDbBkFile,
		Deployment:      config.Deployment,
		CIDR:            serverCIDR,
		Logger:          serverLogger,
	})

	if msg != "" {
		info("wr manager : %s", msg)
	}

	if err != nil {
		die("wr manager failed to start : %s", err)
	}

	logStarted(server.ServerInfo)

	// block forever while the jobqueue does its work
	err = server.Block()
	if err != nil {
		saddr := sAddr(server.ServerInfo)
		jqerr, ok := err.(jobqueue.Error)
		switch {
		case ok && jqerr.Err == jobqueue.ErrClosedTerm:
			info("wr manager on %s gracefully stopped (received SIGTERM)", saddr)
		case ok && jqerr.Err == jobqueue.ErrClosedInt:
			info("wr manager on %s gracefully stopped (received SIGINT)", saddr)
		case ok && jqerr.Err == jobqueue.ErrClosedStop:
			info("wr manager on %s gracefully stopped (following a drain)", saddr)
		default:
			warn("wr manager on %s exited unexpectedly: %s", saddr, err)
		}
	}
}
