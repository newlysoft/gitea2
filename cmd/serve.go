// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unknwon/com"
	"github.com/codegangsta/cli"

	"github.com/go-gitea/gitea/models"
	"github.com/go-gitea/gitea/modules/log"
	"github.com/go-gitea/gitea/modules/setting"
	"github.com/go-gitea/gitea/modules/uuid"
)

const (
	_ACCESS_DENIED_MESSAGE = "Repository does not exist or you do not have access"
)

var CmdServ = cli.Command{
	Name:        "serv",
	Usage:       "This command should only be called by SSH shell",
	Description: `Serv provide access auth for repositories`,
	Action:      runServ,
	Flags: []cli.Flag{
		cli.StringFlag{"config, c", "custom/conf/app.ini", "Custom configuration file path", ""},
	},
}

func setup(logPath string) {
	setting.NewConfigContext()
	log.NewGitLogger(filepath.Join(setting.LogRootPath, logPath))

	if setting.DisableSSH {
		println("Gogs: SSH has been disabled")
		os.Exit(1)
	}

	models.LoadModelsConfig()

	if setting.UseSQLite3 {
		workDir, _ := setting.WorkDir()
		os.Chdir(workDir)
	}

	models.SetEngine()
}

func parseCmd(cmd string) (string, string) {
	ss := strings.SplitN(cmd, " ", 2)
	if len(ss) != 2 {
		return "", ""
	}
	return ss[0], strings.Replace(ss[1], "'/", "'", 1)
}

var (
	COMMANDS = map[string]models.AccessMode{
		"git-upload-pack":    models.ACCESS_MODE_READ,
		"git-upload-archive": models.ACCESS_MODE_READ,
		"git-receive-pack":   models.ACCESS_MODE_WRITE,
	}
)

func runServ(c *cli.Context) {
	if c.IsSet("config") {
		setting.CustomConf = c.String("config")
	}
	setup("serv.log")

	fail := func(userMessage, logMessage string, args ...interface{}) {
		fmt.Fprintln(os.Stderr, "Gogs: ", userMessage)
		log.GitLogger.Fatal(2, logMessage, args...)
	}

	if len(c.Args()) < 1 {
		fail("Not enough arguments", "Not enough arugments")
	}

	keys := strings.Split(c.Args()[0], "-")
	if len(keys) != 2 {
		fail("key-id format error", "Invalid key id: %s", c.Args()[0])
	}

	keyId, err := com.StrTo(keys[1]).Int64()
	if err != nil {
		fail("key-id format error", "Invalid key id: %s", err)
	}

	user, err := models.GetUserByKeyId(keyId)
	if err != nil {
		fail("internal error", "Fail to get user by key ID(%d): %v", keyId, err)
	}

	cmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	if cmd == "" {
		fmt.Printf("Hi, %s! You've successfully authenticated, but Gogs does not provide shell access.\n", user.Name)
		if user.IsAdmin {
			println("If this is unexpected, please log in with password and setup Gogs under another user.")
		}
		return
	}

	verb, args := parseCmd(cmd)
	repoPath := strings.Trim(args, "'")
	rr := strings.SplitN(repoPath, "/", 2)
	if len(rr) != 2 {
		fail("Invalid repository path", "Invalide repository path: %v", args)
	}
	repoUserName := rr[0]
	repoName := strings.TrimSuffix(rr[1], ".git")

	repoUser, err := models.GetUserByName(repoUserName)
	if err != nil {
		if err == models.ErrUserNotExist {
			fail("Repository owner does not exist", "Unregistered owner: %s", repoUserName)
		}
		fail("Internal error", "Fail to get repository owner(%s): %v", repoUserName, err)
	}

	repo, err := models.GetRepositoryByName(repoUser.Id, repoName)
	if err != nil {
		if models.IsErrRepoNotExist(err) {
			if user.Id == repoUser.Id || repoUser.IsOwnedBy(user.Id) {
				fail("Repository does not exist", "Repository does not exist: %s/%s", repoUser.Name, repoName)
			} else {
				fail(_ACCESS_DENIED_MESSAGE, "Repository does not exist: %s/%s", repoUser.Name, repoName)
			}
		}
		fail("Internal error", "Fail to get repository: %v", err)
	}

	requestedMode, has := COMMANDS[verb]
	if !has {
		fail("Unknown git command", "Unknown git command %s", verb)
	}

	mode, err := models.AccessLevel(user, repo)
	if err != nil {
		fail("Internal error", "Fail to check access: %v", err)
	} else if mode < requestedMode {
		clientMessage := _ACCESS_DENIED_MESSAGE
		if mode >= models.ACCESS_MODE_READ {
			clientMessage = "You do not have sufficient authorization for this action"
		}
		fail(clientMessage,
			"User %s does not have level %v access to repository %s",
			user.Name, requestedMode, repoPath)
	}

	uuid := uuid.NewV4().String()
	os.Setenv("uuid", uuid)

	var gitcmd *exec.Cmd
	verbs := strings.Split(verb, " ")
	if len(verbs) == 2 {
		gitcmd = exec.Command(verbs[0], verbs[1], repoPath)
	} else {
		gitcmd = exec.Command(verb, repoPath)
	}
	gitcmd.Dir = setting.RepoRootPath
	gitcmd.Stdout = os.Stdout
	gitcmd.Stdin = os.Stdin
	gitcmd.Stderr = os.Stderr
	if err = gitcmd.Run(); err != nil {
		fail("Internal error", "Fail to execute git command: %v", err)
	}

	if requestedMode == models.ACCESS_MODE_WRITE {
		tasks, err := models.GetUpdateTasksByUuid(uuid)
		if err != nil {
			log.GitLogger.Fatal(2, "GetUpdateTasksByUuid: %v", err)
		}

		for _, task := range tasks {
			err = models.Update(task.RefName, task.OldCommitId, task.NewCommitId,
				user.Name, repoUserName, repoName, user.Id)
			if err != nil {
				log.GitLogger.Error(2, "Fail to update: %v", err)
			}
		}

		if err = models.DelUpdateTasksByUuid(uuid); err != nil {
			log.GitLogger.Fatal(2, "DelUpdateTasksByUuid: %v", err)
		}
	}

	// Update key activity.
	key, err := models.GetPublicKeyById(keyId)
	if err != nil {
		fail("Internal error", "GetPublicKeyById: %v", err)
	}
	key.Updated = time.Now()
	if err = models.UpdatePublicKey(key); err != nil {
		fail("Internal error", "UpdatePublicKey: %v", err)
	}
}
