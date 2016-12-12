// Copyright © 2016 Genome Research Limited
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
	"bufio"
	"github.com/VertebrateResequencing/wr/jobqueue"
	"github.com/pivotal-golang/bytefmt"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// options for this cmd
var reqGroup string
var cmdTime string
var cmdMem string
var cmdCPUs int
var cmdOvr int
var cmdPri int
var cmdRet int
var cmdFile string
var cmdRepGroup string
var cmdDepGroups string
var cmdDeps string

// addCmd represents the add command
var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add commands to the queue",
	Long: `Manually add commands you want run to the queue.

You can supply your commands by putting them in a text file (1 per line), or
by piping them in. In addition to the command itself, you can specify additional
optional tab-separated columns as follows:
cmd cwd req_grp memory time cpus override priority retries rep_grp dep_grps deps
If any of these will be the same for all your commands, you can instead specify
them as flags (which are treated as defaults in the case that they are
unspecified in the text file, but otherwise ignored).

Cwd is the directory to cd to before running the command. If none is specified,
the default will be your current directory right now. (If adding to a remote
cloud-deployed manager, then cwd will instead default to /tmp.)

Req_grp is an arbitrary string that identifies the kind of commands you are
adding, such that future commands you add with this same requirements_group are
likely to have similar memory and time requirements. It defaults to the basename
of the first word in your command, which it assumes to be the name of your
executable.

By providing the memory and time hints, wr manager can do a better job of
spawning runners to handle these commands. The manager learns how much memory
and time commands in the same requirements_group actually used in the past, and
will use its own values unless you set an override. For this learning to work
well, you should have reason to believe that all the commands you add with the
same requirements_group will have similar memory and time requirements, and you
should pick the name in a consistent way such that you'll use it again in the
future.

For example, if you want to run an executable called "exop", and you know that
the memory and time requirements of exop vary with the size of its input file,
you might batch your commands so that all the input files in one batch have
sizes in a certain range, and then provide a requirements_group that describes
this, eg. "exop.1-2G" for inputs in the 1 to 2 GB range.

(Don't name your requirements_group after the expected requirements themselves,
such as "5GB.1hr", because then the manager can't learn about your commands - it
is only learning about how good your estimates are! The name of your executable
should almost always be part of the requirements_group name.)

Override defines if your memory and time should be used instead of the manager's
estimate.
0: do not override wr's learned values for memory and time (if any)
1: override if yours are higher
2: always override

Priority defines how urgent a particular command is; those with higher
priorities will start running before those with lower priorities.

Retries defines how many times a command will be retried automatically if it
fails. Automatic retries are helpful in the case of transient errors, or errors
due to running out of memory or time (when retried, they will be retried with
more memory/time reserved). Once this number of retries is reached, the command
will be "buried" until you take manual action to fix the problem and press the
retry button in the web interface.

Rep_grp is an arbitrary group you can give your commands so you can query their
status later. This is only used for reporting and presentation purposes when
viewing status.

Dep_grps is a comma-separated list of arbitrary names you can associate with a
command, so that you can then refer to this job (and others with the same
dep_grp) in another job's deps.

Deps defines the dependencies of this command. Starting from column 12 you can
specify other commands that must complete before this command will start. The
specification of the other commands is done by having the command line in one
column, and it's cwd in the next, then repeating in subsequent columns for every
other dependency. So a command with 1 dependency would have 13 columns, and one
with 2 dependencies would have 14 columns and so on.
Alternatively, the command slot can be used to specify a comma-separated list of
the dep_grp of other commands, and the cwd slot can be set to the word 'groups'.
In this case, the system will automatically re-run commands if new commands with
the dep_grps they are dependent upon are added to the queue.

NB: Your commands will run with the environment variables you had when you
added them, not the possibly different environment variables you could have in
the future when the commands actually get run.`,
	Run: func(cmd *cobra.Command, args []string) {
		// check the command line options
		if cmdFile == "" {
			die("--file is required")
		}
		if cmdRepGroup == "" {
			cmdRepGroup = "manually_added"
		}
		var cmdMB int
		var err error
		if cmdMem == "" {
			cmdMB = 0
		} else {
			mb, err := bytefmt.ToMegabytes(cmdMem)
			if err != nil {
				die("--memory was not specified correctly: %s", err)
			}
			cmdMB = int(mb)
		}
		var cmdDuration time.Duration
		if cmdTime == "" {
			cmdDuration = 0 * time.Second
		} else {
			cmdDuration, err = time.ParseDuration(cmdTime)
			if err != nil {
				die("--time was not specified correctly: %s", err)
			}
		}
		if cmdCPUs < 1 {
			cmdCPUs = 1
		}
		if cmdOvr < 0 || cmdOvr > 2 {
			die("--override must be in the range 0..2")
		}
		if cmdPri < 0 || cmdPri > 255 {
			die("--priority must be in the range 0..255")
		}
		if cmdRet < 0 || cmdRet > 255 {
			die("--retries must be in the range 0..255")
		}
		timeout := time.Duration(timeoutint) * time.Second

		var defaultDepGroups []string
		if cmdDepGroups != "" {
			defaultDepGroups = strings.Split(cmdDepGroups, ",")
		}

		var defaultDeps []*jobqueue.Dependency
		if cmdDeps != "" {
			cols := strings.Split(cmdDeps, "\\t")
			if len(cols)%2 != 0 {
				die("--deps must have an even number of tab-separated columns")
			}
			defaultDeps = colsToDeps(cols)
		}

		// open file or set up to read from STDIN
		var reader io.Reader
		if cmdFile == "-" {
			reader = os.Stdin
		} else {
			reader, err = os.Open(cmdFile)
			if err != nil {
				die("could not open file '%s': %s", cmdFile, err)
			}
			defer reader.(*os.File).Close()
		}

		// we'll default to pwd if the manager is on the same host as us, /tmp
		// otherwise
		jq, err := jobqueue.Connect(addr, "cmds", timeout)
		if err != nil {
			die("%s", err)
		}
		sstats, err := jq.ServerStats()
		if err != nil {
			die("even though I was able to connect to the manager, it failed to tell me its location")
		}
		var pwd string
		var pwdWarning int
		if jobqueue.CurrentIP()+":"+config.ManagerPort == sstats.ServerInfo.Addr {
			pwd, err = os.Getwd()
			if err != nil {
				die("%s", err)
			}
		} else {
			pwd = "/tmp"
			pwdWarning = 1
		}
		jq.Disconnect()

		// for network efficiency, read in all commands and create a big slice
		// of Jobs and Add() them in one go afterwards
		var jobs []*jobqueue.Job
		scanner := bufio.NewScanner(reader)
		defaultedRepG := false
		for scanner.Scan() {
			cols := strings.Split(scanner.Text(), "\t")
			colsn := len(cols)
			if colsn < 1 || cols[0] == "" {
				continue
			}

			var cmd, cwd, rg, repg string
			var mb, cpus, override, priority, retries int
			var dur time.Duration
			var depGroups []string
			var deps *jobqueue.Dependencies

			// cmd cwd requirements_group memory time cpus override priority retries id deps
			cmd = cols[0]

			if colsn < 2 || cols[1] == "" {
				if cmdCwd != "" {
					cwd = cmdCwd
				} else {
					if pwdWarning == 1 {
						warn("command working directories defaulting to /tmp since the manager is running remotely")
						pwdWarning = 0
					}
					cwd = pwd
				}
			} else {
				cwd = cols[1]
			}

			if colsn < 3 || cols[2] == "" {
				if reqGroup != "" {
					rg = reqGroup
				} else {
					parts := strings.Split(cmd, " ")
					rg = filepath.Base(parts[0])
				}
			} else {
				rg = cols[2]
			}

			if colsn < 4 || cols[3] == "" {
				mb = cmdMB
			} else {
				thismb, err := bytefmt.ToMegabytes(cols[3])
				if err != nil {
					die("a value in the memory column (%s) was not specified correctly: %s", cols[3], err)
				}
				mb = int(thismb)
			}

			if colsn < 5 || cols[4] == "" {
				dur = cmdDuration
			} else {
				dur, err = time.ParseDuration(cols[4])
				if err != nil {
					die("a value in the time column (%s) was not specified correctly: %s", cols[4], err)
				}
			}

			if colsn < 6 || cols[5] == "" {
				cpus = cmdCPUs
			} else {
				cpus, err = strconv.Atoi(cols[5])
				if err != nil {
					die("a value in the cpus column (%s) was not specified correctly: %s", cols[5], err)
				}
			}

			if colsn < 7 || cols[6] == "" {
				override = cmdOvr
			} else {
				override, err = strconv.Atoi(cols[6])
				if err != nil {
					die("a value in the override column (%s) was not specified correctly: %s", cols[6], err)
				}
				if override < 0 || override > 2 {
					die("override column must contain values in the range 0..2 (not %d)", override)
				}
			}

			if colsn < 8 || cols[7] == "" {
				priority = cmdPri
			} else {
				priority, err = strconv.Atoi(cols[7])
				if err != nil {
					die("a value in the priority column (%s) was not specified correctly: %s", cols[7], err)
				}
				if priority < 0 || priority > 255 {
					die("priority column must contain values in the range 0..255 (not %d)", priority)
				}
			}

			if colsn < 9 || cols[8] == "" {
				retries = cmdRet
			} else {
				retries, err = strconv.Atoi(cols[8])
				if err != nil {
					die("a value in the retries column (%s) was not specified correctly: %s", cols[8], err)
				}
				if priority < 0 || priority > 255 {
					die("retries column must contain values in the range 0..255 (not %d)", retries)
				}
			}

			if colsn < 10 || cols[9] == "" {
				repg = cmdRepGroup
				defaultedRepG = true
			} else {
				repg = cols[9]
			}

			if colsn < 11 || cols[10] == "" {
				depGroups = defaultDepGroups
			} else {
				depGroups = strings.Split(cols[10], ",")
			}

			if colsn < 12 || cols[11] == "" {
				deps = jobqueue.NewDependencies(defaultDeps...)
			} else {
				// all remaining columns specify deps
				depCols := cols[11:]
				if len(depCols)%2 != 0 {
					die("there must be an even number of dependency columns")
				}
				deps = jobqueue.NewDependencies(colsToDeps(depCols)...)
			}

			jobs = append(jobs, jobqueue.NewJob(cmd, cwd, rg, mb, dur, cpus, uint8(override), uint8(priority), uint8(retries), repg, depGroups, deps))
		}

		// connect to the server
		jq, err = jobqueue.Connect(addr, "cmds", timeout)
		if err != nil {
			die("%s", err)
		}
		defer jq.Disconnect()

		// add the jobs to the queue
		inserts, dups, err := jq.Add(jobs)
		if err != nil {
			die("%s", err)
		}

		if defaultedRepG {
			info("Added %d new commands (%d were duplicates) to the queue using default identifier '%s'", inserts, dups, cmdRepGroup)
		} else {
			info("Added %d new commands (%d were duplicates) to the queue", inserts, dups)
		}
	},
}

func init() {
	RootCmd.AddCommand(addCmd)

	// flags specific to this sub-command
	addCmd.Flags().StringVarP(&cmdFile, "file", "f", "-", "file containing your commands; - means read from STDIN")
	addCmd.Flags().StringVarP(&cmdRepGroup, "report_grp", "i", "manually_added", "reporting group for your commands")
	addCmd.Flags().StringVarP(&cmdDepGroups, "dep_grps", "e", "", "comma-separated list of dependency groups")
	addCmd.Flags().StringVarP(&cmdCwd, "cwd", "c", "", "working dir")
	addCmd.Flags().StringVarP(&reqGroup, "req_grp", "g", "", "group name for commands with similar reqs")
	addCmd.Flags().StringVarP(&cmdMem, "memory", "m", "1G", "peak mem est. [specify units such as M for Megabytes or G for Gigabytes]")
	addCmd.Flags().StringVarP(&cmdTime, "time", "t", "1h", "max time est. [specify units such as m for minutes or h for hours]")
	addCmd.Flags().IntVar(&cmdCPUs, "cpus", 1, "cpu cores needed")
	addCmd.Flags().IntVarP(&cmdOvr, "override", "o", 0, "[0|1|2] should your mem/time estimates override?")
	addCmd.Flags().IntVarP(&cmdPri, "priority", "p", 0, "[0-255] command priority")
	addCmd.Flags().IntVarP(&cmdRet, "retries", "r", 3, "[0-255] number of automatic retries for failed commands")
	addCmd.Flags().StringVarP(&cmdDeps, "deps", "d", "", "dependencies of your commands, in the form \"command1\\tcwd1\\tcommand2\\tcwd2...\" or \"dep_grp1,dep_grp2...\\tgroups\"")

	addCmd.Flags().IntVar(&timeoutint, "timeout", 30, "how long (seconds) to wait to get a reply from 'wr manager'")
}

// convert cmd,cwd or depgroups,"groups" columns in to Dependency
func colsToDeps(cols []string) (deps []*jobqueue.Dependency) {
	for i := 0; i < len(cols); i += 2 {
		if cols[i+1] == "groups" {
			for _, depgroup := range strings.Split(cols[i], ",") {
				deps = append(deps, jobqueue.NewDepGroupDependency(depgroup))
			}
		} else {
			deps = append(deps, jobqueue.NewCmdDependency(cols[i], cols[i+1]))
		}
	}
	return
}
