/*
Copyright © 2021 windvalley

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package sshtask

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ScaleFT/sshkeys"
	"github.com/go-project-pkg/expandhost"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"

	"github.com/windvalley/gossh/internal/cmd/vault"
	"github.com/windvalley/gossh/internal/pkg/aes"
	"github.com/windvalley/gossh/internal/pkg/configflags"
	"github.com/windvalley/gossh/pkg/batchssh"
	"github.com/windvalley/gossh/pkg/log"
	"github.com/windvalley/gossh/pkg/util"
)

var (
	linuxUserRegex  = "[a-zA-Z0-9_.-]+[$]?"
	sudoPromptRegex = fmt.Sprintf(
		`(?s).*\[sudo\] password for %s: \n|(?s).*\[sudo\] %s 的密码：\n`,
		linuxUserRegex,
		linuxUserRegex,
	)
)

// TaskType ...
type TaskType int

// ...
const (
	CommandTask TaskType = iota
	ScriptTask
	PushTask
	FetchTask
)

// taskResult ...
type taskResult struct {
	taskID            string
	hostsSuccessCount int
	hostsFailureCount int
	elapsed           float64
}

// detailResult each ssh host result.
type detailResult struct {
	taskID   string
	hostname string
	status   string
	output   string
}

type pushFiles struct {
	files    []string
	zipFiles []string
}

// Task ...
type Task struct {
	configFlags *configflags.ConfigFlags

	id        string
	taskType  TaskType
	sshClient *batchssh.Client
	sshAgent  net.Conn

	// hostnames or ips from command line arguments.
	hosts []string

	command    string
	scriptFile string

	pushFiles      *pushFiles
	fetchFiles     []string
	dstDir         string
	tmpDir         string
	remove         bool
	allowOverwrite bool

	taskOutput   chan taskResult
	detailOutput chan detailResult

	err error
}

// NewTask ...
func NewTask(taskType TaskType, configFlags *configflags.ConfigFlags) *Task {
	return &Task{
		configFlags:  configFlags,
		id:           time.Now().Format("20060102150405"),
		taskType:     taskType,
		taskOutput:   make(chan taskResult, 1),
		detailOutput: make(chan detailResult),
	}
}

// Start task.
func (t *Task) Start() {
	if t.sshAgent != nil {
		defer t.sshAgent.Close()
	}

	go func() {
		defer close(t.taskOutput)
		defer close(t.detailOutput)
		t.BatchRun()
	}()

	taskTimeout := t.configFlags.Timeout.Task
	if taskTimeout > 0 {
		go func() {
			time.Sleep(time.Duration(taskTimeout) * time.Second)
			log.Warnf(
				"task timeout, taskID: %s, timeout value: %d seconds",
				t.id,
				taskTimeout,
			)
			close(t.detailOutput)
			close(t.taskOutput)
		}()
	}

	t.HandleOutput()
}

// SetTargetHosts ...
func (t *Task) SetTargetHosts(hosts []string) {
	t.hosts = hosts
}

// SetCommand ...
func (t *Task) SetCommand(command string) {
	t.command = command
}

// SetScriptFile ...
func (t *Task) SetScriptFile(sciptFile string) {
	t.scriptFile = sciptFile
}

// SetPushfiles ...
func (t *Task) SetPushfiles(files, zipFiles []string) {
	t.pushFiles = &pushFiles{
		files:    files,
		zipFiles: zipFiles,
	}
}

// SetFetchFiles ...
func (t *Task) SetFetchFiles(files []string) {
	t.fetchFiles = files
}

// SetScriptOptions ...
func (t *Task) SetScriptOptions(destPath string, remove, allowOverwrite bool) {
	t.dstDir = destPath
	t.remove = remove
	t.allowOverwrite = allowOverwrite
}

// SetPushOptions ...
func (t *Task) SetPushOptions(destPath string, allowOverwrite bool) {
	t.dstDir = destPath
	t.allowOverwrite = allowOverwrite
}

// SetFetchOptions ...
func (t *Task) SetFetchOptions(destPath, tmpDir string) {
	t.dstDir = destPath
	t.tmpDir = tmpDir
}

// RunSSH implements batchssh.Task
func (t *Task) RunSSH(addr string) (string, error) {
	lang := t.configFlags.Run.Lang
	runAs := t.configFlags.Run.AsUser
	sudo := t.configFlags.Run.Sudo

	switch t.taskType {
	case CommandTask:
		return t.sshClient.ExecuteCmd(addr, t.command, lang, runAs, sudo)
	case ScriptTask:
		return t.sshClient.ExecuteScript(addr, t.scriptFile, t.dstDir, lang, runAs, sudo, t.remove, t.allowOverwrite)
	case PushTask:
		return t.sshClient.PushFiles(addr, t.pushFiles.files, t.pushFiles.zipFiles, t.dstDir, t.allowOverwrite)
	case FetchTask:
		return t.sshClient.FetchFiles(addr, t.fetchFiles, t.dstDir, t.tmpDir, sudo, runAs)
	default:
		return "", fmt.Errorf("unknown task type: %v", t.taskType)
	}
}

//nolint:gocyclo
// BatchRun ...
func (t *Task) BatchRun() {
	timeNow := time.Now()

	allHosts, err := t.getAllHosts()
	if err != nil {
		t.err = err
		return
	}

	log.Debugf("got target hosts, count: %d", len(allHosts))

	if t.configFlags.Hosts.List {
		hostsCount := len(allHosts)
		fmt.Printf("%s\n", strings.Join(allHosts, "\n"))
		fmt.Fprintf(os.Stderr, "\nhosts (%d)\n", hostsCount)
		return
	}

	authConf := t.configFlags.Auth
	runConf := t.configFlags.Run

	log.Debugf("Auth: login user: %s", authConf.User)

	if runConf.Sudo {
		log.Debugf("Auth: use sudo as user '%s'", runConf.AsUser)
	}

	switch t.taskType {
	case CommandTask:
		if t.command == "" {
			t.err = errors.New("need flag '-e/--execute' or '-L/--hosts.list'")
		}
	case ScriptTask:
		if t.scriptFile == "" {
			t.err = errors.New("need flag '-e/--execute' or '-L/--hosts.list'")
		}
	case PushTask:
		if t.pushFiles == nil || len(t.pushFiles.files) == 0 {
			t.err = errors.New("need flag '-f/--files' or '-L/--hosts.list'")
		}
	case FetchTask:
		if len(t.fetchFiles) == 0 {
			t.err = errors.New("need flag '-f/--files' or '-L/--hosts.list'")
		} else if len(t.dstDir) == 0 {
			t.err = errors.New("need flag '-d/--dest-path' or '-L/--hosts.list'")
		} else {
			if !util.DirExists(t.dstDir) {
				err := os.MkdirAll(t.dstDir, os.ModePerm)
				util.CheckErr(err)
			}
		}
	}

	if t.err != nil {
		return
	}

	t.buildSSHClient()

	result := t.sshClient.BatchRun(allHosts, t)
	successCount, failedCount := 0, 0
	for v := range result {
		if v.Status == batchssh.SuccessIdentifier {
			successCount++
		} else {
			failedCount++
		}

		t.detailOutput <- detailResult{
			taskID:   t.id,
			hostname: v.Addr,
			status:   v.Status,
			output:   v.Message,
		}
	}

	elapsed := time.Since(timeNow).Seconds()

	t.taskOutput <- taskResult{
		t.id,
		successCount,
		failedCount,
		elapsed,
	}
}

// HandleOutput ...
func (t *Task) HandleOutput() {
	for res := range t.detailOutput {
		output := ""

		// Fix the problem of special characters ^M appearing at the end of
		// the line break when writing files in text format.
		outputNoR := strings.ReplaceAll(res.output, "\r\n", "\n")

		// Trim leading and trailing blank characters.
		outputNoSpace := strings.TrimSpace(outputNoR)

		// Trim sudo password prompt messages.
		re, err := regexp.Compile(sudoPromptRegex)
		if err != nil {
			log.Debugf("re compile '%s' failed: %s", sudoPromptRegex, err)
		} else {
			output = re.ReplaceAllString(outputNoSpace, "")
		}

		contextLogger := log.WithFields(log.Fields{
			"hostname": res.hostname,
			"status":   res.status,
			"output":   output,
		})

		if res.status == batchssh.SuccessIdentifier {
			contextLogger.Infof("success")
		} else {
			contextLogger.Errorf("failed")
		}
	}

	for res := range t.taskOutput {
		log.Infof(
			"success count: %d, failed count: %d, elapsed: %.2fs",
			res.hostsSuccessCount,
			res.hostsFailureCount,
			res.elapsed,
		)
	}
}

// CheckErr ...
func (t *Task) CheckErr() error {
	return t.err
}

func (t *Task) getAllHosts() ([]string, error) {
	var hosts []string

	if len(t.hosts) != 0 {
		for _, hostOrPattern := range t.hosts {
			hostOrPattern = strings.TrimSpace(hostOrPattern)

			if hostOrPattern == "" {
				continue
			}

			hostList, err := expandhost.PatternToHosts(hostOrPattern)
			if err != nil {
				return nil, fmt.Errorf("invalid host pattern: %s", err)
			}

			hosts = append(hosts, hostList...)
		}
	}

	if t.configFlags.Hosts.File != "" {
		content, err := ioutil.ReadFile(t.configFlags.Hosts.File)
		if err != nil {
			return nil, fmt.Errorf("read hosts file failed: %s", err)
		}

		hostSlice := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
		for _, hostOrPattern := range hostSlice {
			hostOrPattern = strings.TrimSpace(hostOrPattern)

			if hostOrPattern == "" {
				continue
			}

			hostList, err := expandhost.PatternToHosts(hostOrPattern)
			if err != nil {
				return nil, fmt.Errorf("invalid host pattern: %s", err)
			}

			hosts = append(hosts, hostList...)
		}
	}

	if len(hosts) == 0 {
		return nil, fmt.Errorf("need target hosts, you can specify hosts file by flag '-H' or " +
			"provide host/pattern as positional arguments")
	}

	return util.RemoveDuplStr(hosts), nil
}

func (t *Task) buildSSHClient() {
	var sshClient *batchssh.Client

	password, err := t.getPassword()
	if err != nil {
		util.CheckErr(err)
	}

	auths := t.getSSHAuthMethods(&password)

	if t.configFlags.Proxy.Server != "" {
		proxyAuths := t.getProxySSHAuthMethods(&password)

		sshClient = batchssh.NewClient(
			t.configFlags.Auth.User,
			password,
			auths,
			batchssh.WithConnTimeout(time.Duration(t.configFlags.Timeout.Conn)*time.Second),
			batchssh.WithCommandTimeout(time.Duration(t.configFlags.Timeout.Command)*time.Second),
			batchssh.WithConcurrency(t.configFlags.Run.Concurrency),
			batchssh.WithPort(t.configFlags.Hosts.Port),
			batchssh.WithProxyServer(
				t.configFlags.Proxy.Server,
				t.configFlags.Proxy.User,
				t.configFlags.Proxy.Port,
				proxyAuths,
			),
		)
	} else {
		sshClient = batchssh.NewClient(
			t.configFlags.Auth.User,
			password,
			auths,
			batchssh.WithConnTimeout(time.Duration(t.configFlags.Timeout.Conn)*time.Second),
			batchssh.WithCommandTimeout(time.Duration(t.configFlags.Timeout.Command)*time.Second),
			batchssh.WithConcurrency(t.configFlags.Run.Concurrency),
			batchssh.WithPort(t.configFlags.Hosts.Port),
		)
	}

	t.sshClient = sshClient
}

func (t *Task) getSSHAuthMethods(password *string) []ssh.AuthMethod {
	var (
		auths    []ssh.AuthMethod
		sshAgent net.Conn
		err      error
	)

	if *password != "" {
		auths = append(auths, ssh.Password(*password))
	} else {
		log.Debugf("Auth: password of the login user not provided")
	}

	keyfiles := t.getItentityFiles()
	if len(keyfiles) != 0 {
		sshSigners := getSigners(keyfiles, t.configFlags.Auth.Passphrase, false)
		if len(sshSigners) == 0 {
			log.Debugf("Auth: no valid identity files")
		} else {
			auths = append(auths, ssh.PublicKeys(sshSigners...))
		}
	}

	sshAuthSock := os.Getenv("SSH_AUTH_SOCK")
	if sshAuthSock != "" {
		sshAgent, err = net.Dial("unix", sshAuthSock)
		if err != nil {
			log.Debugf("Auth: connect ssh-agent failed: %s", err)
		} else {
			log.Debugf("Auth: connected to SSH_AUTH_SOCK: %s", sshAuthSock)

			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers))
		}

		t.sshAgent = sshAgent
	}

	if len(auths) == 0 {
		log.Debugf("Auth: no valid authentication method detected. Prompt for password of the login user")

		*password = getPasswordFromPrompt(t.configFlags.Auth.User)
		auths = append(auths, ssh.Password(*password))

		return auths
	}

	if *password == "" && t.configFlags.Run.Sudo {
		log.Debugf("Auth: using sudo as other user needs password. Prompt for password of the login user")

		*password = getPasswordFromPrompt(t.configFlags.Auth.User)
		auths = append(auths, ssh.Password(*password))
	}

	return auths
}

func (t *Task) getProxySSHAuthMethods(password *string) []ssh.AuthMethod {
	var (
		proxyAuths []ssh.AuthMethod
		sshAgent   net.Conn
		err        error
	)

	log.Debugf("Proxy Auth: proxy login user: %s", t.configFlags.Proxy.User)

	if t.configFlags.Proxy.Password != "" {
		proxyAuths = append(proxyAuths, ssh.Password(t.configFlags.Proxy.Password))
	} else {
		proxyAuths = append(proxyAuths, ssh.Password(*password))
	}
	log.Debugf("Proxy Auth: received password of the proxy user")

	proxyKeyfiles := t.getProxyItentityFiles()
	if len(proxyKeyfiles) != 0 {
		sshSigners := getSigners(proxyKeyfiles, t.configFlags.Proxy.Passphrase, true)
		if len(sshSigners) == 0 {
			log.Debugf("Proxy Auth: no valid identity files for proxy")
		} else {
			proxyAuths = append(proxyAuths, ssh.PublicKeys(sshSigners...))
		}
	}

	sshAuthSock := os.Getenv("SSH_AUTH_SOCK")
	if sshAuthSock != "" {
		sshAgent, err = net.Dial("unix", sshAuthSock)
		if err != nil {
			log.Debugf("Proxy Auth: connect ssh-agent failed: %s", err)
		} else {
			log.Debugf("Proxy Auth: connected to SSH_AUTH_SOCK: %s", sshAuthSock)

			agentSigners := ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers)
			proxyAuths = append(proxyAuths, agentSigners)
		}

		t.sshAgent = sshAgent
	}

	return proxyAuths
}

func (t *Task) getPassword() (password string, err error) {
	authFile := t.configFlags.Auth.PassFile
	if authFile != "" {
		var passwordContent []byte

		passwordContent, err = ioutil.ReadFile(authFile)
		if err != nil {
			err = fmt.Errorf("read auth file '%s' failed: %w", authFile, err)
		}

		password = strings.TrimSpace(string(passwordContent))

		log.Debugf("Auth: read password from file '%s'", authFile)
	}

	passwordFromFlag := t.configFlags.Auth.Password
	if passwordFromFlag != "" {
		password = passwordFromFlag

		log.Debugf("Auth: received password from commandline flag or configuration file")
	}

	assignRealPass(&password)

	if t.configFlags.Auth.AskPass {
		log.Debugf("Auth: ask for password of login user by flag '-k/--auth.ask-pass'")
		password = getPasswordFromPrompt(t.configFlags.Auth.User)
	}

	//nolint:nakedret
	return
}

func (t *Task) getItentityFiles() (keyFiles []string) {
	homeDir := os.Getenv("HOME")
	for _, file := range t.configFlags.Auth.IdentityFiles {
		if strings.HasPrefix(file, "~/") {
			file = strings.Replace(file, "~", homeDir, 1)
		}

		keyFiles = append(keyFiles, file)
	}

	return
}

func (t *Task) getProxyItentityFiles() (proxyKeyfiles []string) {
	homeDir := os.Getenv("HOME")
	for _, file := range t.configFlags.Proxy.IdentityFiles {
		if strings.HasPrefix(file, "~/") {
			file = strings.Replace(file, "~", homeDir, 1)
		}

		proxyKeyfiles = append(proxyKeyfiles, file)
	}

	return
}

func getSigners(keyfiles []string, passphrase string, isForProxy bool) []ssh.Signer {
	assignRealPass(&passphrase)

	var (
		signers []ssh.Signer
		msgHead string
	)

	if isForProxy {
		msgHead = "Proxy Auth: "
	} else {
		msgHead = "Auth: "
	}

	for _, f := range keyfiles {
		signer, msg := getSigner(f, passphrase)

		log.Debugf("%s%s", msgHead, msg)

		if signer != nil {
			signers = append(signers, signer)
		}
	}

	return signers
}

func getSigner(keyfile, passphrase string) (ssh.Signer, string) {
	buf, err := ioutil.ReadFile(keyfile)
	if err != nil {
		return nil, fmt.Sprintf("read identity file '%s' failed: %s", keyfile, err)
	}

	pubkey, err := ssh.ParsePrivateKey(buf)
	if err != nil {
		_, ok := err.(*ssh.PassphraseMissingError)
		if ok {
			pubkeyWithPassphrase, err1 := sshkeys.ParseEncryptedPrivateKey(buf, []byte(passphrase))
			if err1 != nil {
				return nil, fmt.Sprintf("parse identity file '%s' with passphrase failed: %s", keyfile, err1)
			}

			return pubkeyWithPassphrase, fmt.Sprintf("parsed identity file '%s' with passphrase", keyfile)
		}

		return nil, fmt.Sprintf("parse identity file '%s' failed: %s", keyfile, err)
	}

	return pubkey, fmt.Sprintf("parsed identity file '%s'", keyfile)
}

func getPasswordFromPrompt(loginUser string) string {
	fmt.Fprintf(os.Stderr, "Password for %s: ", loginUser)

	var passwordByte []byte
	passwordByte, err := term.ReadPassword(0)
	if err != nil {
		err = fmt.Errorf("get password from terminal failed: %s", err)
	}
	util.CheckErr(err)

	password := string(passwordByte)

	fmt.Println("")

	log.Debugf("Auth: received password of the login user '%s' from terminal prompt", loginUser)

	return password
}

func assignRealPass(pass *string) {
	var err error

	if aes.IsAES256CipherText(*pass) {
		vaultPass := vault.GetVaultPassword()

		*pass, err = aes.AES256Decode(*pass, vaultPass)
		if err != nil {
			log.Debugf("Auth: decrypt password/passphrase which encrypted by vault failed: %s", err)
			util.CheckErr(err)
		}

		log.Debugf("Auth: decrypt password/passphrase which encrypted by vault success")
	}
}
