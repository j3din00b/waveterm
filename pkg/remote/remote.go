package remote

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/scripthaus-dev/mshell/pkg/base"
	"github.com/scripthaus-dev/mshell/pkg/packet"
	"github.com/scripthaus-dev/mshell/pkg/shexec"
	"github.com/scripthaus-dev/sh2-server/pkg/sstore"
)

const RemoteTypeMShell = "mshell"
const DefaultTermRows = 25
const DefaultTermCols = 80
const DefaultTerm = "xterm-256color"

const MShellServerCommand = `
PATH=$PATH:~/.mshell;
which mshell > /dev/null;
if [[ "$?" -ne 0 ]]
then
  printf "\n##N{\"type\": \"init\", \"notfound\": true, \"uname\": \"%s | %s\"}\n" "$(uname -s)" "$(uname -m)"
else
  mshell --server
fi
`

const (
	StatusInit         = "init"
	StatusConnected    = "connected"
	StatusDisconnected = "disconnected"
	StatusError        = "error"
)

var GlobalStore *Store

type Store struct {
	Lock              *sync.Mutex
	Map               map[string]*MShellProc // key=remoteid
	CmdStatusCallback func(ck base.CommandKey, status string)
}

type RemoteState struct {
	RemoteType          string              `json:"remotetype"`
	RemoteId            string              `json:"remoteid"`
	PhysicalId          string              `json:"physicalremoteid"`
	RemoteAlias         string              `json:"remotealias"`
	RemoteCanonicalName string              `json:"remotecanonicalname"`
	RemoteVars          map[string]string   `json:"remotevars"`
	Status              string              `json:"status"`
	DefaultState        *sstore.RemoteState `json:"defaultstate"`
}

type MShellProc struct {
	Lock   *sync.Mutex
	Remote *sstore.RemoteType

	// runtime
	Status     string
	ServerProc *shexec.ClientProc
	Err        error

	RunningCmds []base.CommandKey
}

func LoadRemotes(ctx context.Context) error {
	GlobalStore = &Store{
		Lock: &sync.Mutex{},
		Map:  make(map[string]*MShellProc),
	}
	allRemotes, err := sstore.GetAllRemotes(ctx)
	if err != nil {
		return err
	}
	for _, remote := range allRemotes {
		msh := MakeMShell(remote)
		GlobalStore.Map[remote.RemoteId] = msh
		if remote.AutoConnect {
			go msh.Launch()
		}
	}
	return nil
}

func GetRemoteByName(name string) *MShellProc {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()
	for _, msh := range GlobalStore.Map {
		if msh.Remote.RemoteAlias == name || msh.Remote.GetName() == name {
			return msh
		}
	}
	return nil
}

func GetRemoteById(remoteId string) *MShellProc {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()
	return GlobalStore.Map[remoteId]
}

func GetAllRemoteState() []RemoteState {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()

	var rtn []RemoteState
	for _, proc := range GlobalStore.Map {
		state := RemoteState{
			RemoteType:          proc.Remote.RemoteType,
			RemoteId:            proc.Remote.RemoteId,
			RemoteAlias:         proc.Remote.RemoteAlias,
			RemoteCanonicalName: proc.Remote.RemoteCanonicalName,
			PhysicalId:          proc.Remote.PhysicalId,
			Status:              proc.Status,
		}
		vars := make(map[string]string)
		vars["user"] = proc.Remote.RemoteUser
		vars["host"] = proc.Remote.RemoteHost
		if proc.Remote.RemoteSudo {
			vars["sudo"] = "1"
		}
		vars["alias"] = proc.Remote.RemoteAlias
		vars["cname"] = proc.Remote.RemoteCanonicalName
		vars["physicalid"] = proc.Remote.PhysicalId
		vars["remoteid"] = proc.Remote.RemoteId
		vars["status"] = proc.Status
		vars["type"] = proc.Remote.RemoteType
		if proc.ServerProc != nil && proc.ServerProc.InitPk != nil {
			state.DefaultState = &sstore.RemoteState{Cwd: proc.ServerProc.InitPk.HomeDir}
			vars["home"] = proc.ServerProc.InitPk.HomeDir
			vars["remoteuser"] = proc.ServerProc.InitPk.User
			vars["remotehost"] = proc.ServerProc.InitPk.HostName
			if proc.Remote.SSHOpts == nil || proc.Remote.SSHOpts.SSHHost == "" {
				vars["local"] = "1"
			}
		}
		state.RemoteVars = vars
		rtn = append(rtn, state)
	}
	return rtn
}

func GetDefaultRemoteStateById(remoteId string) (*sstore.RemoteState, error) {
	remote := GetRemoteById(remoteId)
	if remote == nil {
		return nil, fmt.Errorf("remote not found")
	}
	if !remote.IsConnected() {
		return nil, fmt.Errorf("remote not connected")
	}
	state := remote.GetDefaultState()
	if state == nil {
		return nil, fmt.Errorf("could not get default remote state")
	}
	return state, nil
}

func MakeMShell(r *sstore.RemoteType) *MShellProc {
	rtn := &MShellProc{Lock: &sync.Mutex{}, Remote: r, Status: StatusInit}
	return rtn
}

func convertSSHOpts(opts *sstore.SSHOpts) shexec.SSHOpts {
	if opts == nil {
		return shexec.SSHOpts{}
	}
	return shexec.SSHOpts{
		SSHHost:     opts.SSHHost,
		SSHOptsStr:  opts.SSHOptsStr,
		SSHIdentity: opts.SSHIdentity,
		SSHUser:     opts.SSHUser,
	}
}

func (msh *MShellProc) Launch() {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()

	ecmd := convertSSHOpts(msh.Remote.SSHOpts).MakeSSHExecCmd(MShellServerCommand)
	cproc, err := shexec.MakeClientProc(ecmd)
	if err != nil {
		msh.Status = StatusError
		msh.Err = err
		return
	}
	msh.ServerProc = cproc
	msh.Status = StatusConnected
	go func() {
		exitErr := cproc.Cmd.Wait()
		exitCode := shexec.GetExitCode(exitErr)
		msh.WithLock(func() {
			if msh.Status == StatusConnected {
				msh.Status = StatusDisconnected
			}
		})

		fmt.Printf("[error] RUNNER PROC EXITED code[%d]\n", exitCode)
	}()
	go msh.ProcessPackets()
	return
}

func (msh *MShellProc) IsConnected() bool {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()
	return msh.Status == StatusConnected
}

func (msh *MShellProc) GetDefaultState() *sstore.RemoteState {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()
	if msh.ServerProc == nil || msh.ServerProc.InitPk == nil {
		return nil
	}
	return &sstore.RemoteState{Cwd: msh.ServerProc.InitPk.HomeDir}
}

func (msh *MShellProc) ExpandHomeDir(pathStr string) (string, error) {
	if pathStr != "~" && !strings.HasPrefix(pathStr, "~/") {
		return pathStr, nil
	}
	msh.Lock.Lock()
	defer msh.Lock.Unlock()
	if msh.ServerProc.InitPk == nil {
		return "", fmt.Errorf("remote not connected, does not have home directory set for ~ expansion")
	}
	homeDir := msh.ServerProc.InitPk.HomeDir
	if homeDir == "" {
		return "", fmt.Errorf("remote does not have HOME set, cannot do ~ expansion")
	}
	if pathStr == "~" {
		return homeDir, nil
	}
	return path.Join(homeDir, pathStr[2:]), nil
}

func (msh *MShellProc) IsCmdRunning(ck base.CommandKey) bool {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()
	for _, runningCk := range msh.RunningCmds {
		if runningCk == ck {
			return true
		}
	}
	return false
}

func (msh *MShellProc) SendInput(pk *packet.InputPacketType) error {
	if !msh.IsConnected() {
		return fmt.Errorf("remote is not connected, cannot send input")
	}
	if !msh.IsCmdRunning(pk.CK) {
		return fmt.Errorf("cannot send input, cmd is not running")
	}
	dataPk := packet.MakeDataPacket()
	dataPk.CK = pk.CK
	dataPk.FdNum = 0 // stdin
	dataPk.Data64 = pk.InputData64
	return msh.ServerProc.Input.SendPacket(dataPk)
}

func makeTermOpts() sstore.TermOpts {
	return sstore.TermOpts{Rows: DefaultTermRows, Cols: DefaultTermCols, FlexRows: true}
}

func RunCommand(ctx context.Context, cmdId string, remoteId string, remoteState *sstore.RemoteState, runPacket *packet.RunPacketType) (*sstore.CmdType, error) {
	msh := GetRemoteById(remoteId)
	if msh == nil {
		return nil, fmt.Errorf("no remote id=%s found", remoteId)
	}
	if !msh.IsConnected() {
		return nil, fmt.Errorf("remote '%s' is not connected", remoteId)
	}
	if remoteState == nil {
		return nil, fmt.Errorf("no remote state passed to RunCommand")
	}
	fmt.Printf("RUN-CMD> %s reqid=%s (msh=%v)\n", runPacket.CK, runPacket.ReqId, msh.Remote)
	msh.ServerProc.Output.RegisterRpc(runPacket.ReqId)
	err := shexec.SendRunPacketAndRunData(ctx, msh.ServerProc.Input, runPacket)
	if err != nil {
		return nil, fmt.Errorf("sending run packet to remote: %w", err)
	}
	rtnPk := msh.ServerProc.Output.WaitForResponse(ctx, runPacket.ReqId)
	startPk, ok := rtnPk.(*packet.CmdStartPacketType)
	if !ok {
		respPk, ok := rtnPk.(*packet.ResponsePacketType)
		if !ok {
			return nil, fmt.Errorf("invalid response received from server for run packet: %s", packet.AsString(rtnPk))
		}
		if respPk.Error != "" {
			return nil, errors.New(respPk.Error)
		}
		return nil, fmt.Errorf("invalid response received from server for run packet: %s", packet.AsString(rtnPk))
	}
	status := sstore.CmdStatusRunning
	if runPacket.Detached {
		status = sstore.CmdStatusDetached
	}
	cmd := &sstore.CmdType{
		SessionId:   runPacket.CK.GetSessionId(),
		CmdId:       startPk.CK.GetCmdId(),
		CmdStr:      runPacket.Command,
		RemoteId:    msh.Remote.RemoteId,
		RemoteState: *remoteState,
		TermOpts:    makeTermOpts(),
		Status:      status,
		StartPk:     startPk,
		DonePk:      nil,
		RunOut:      nil,
	}
	err = sstore.AppendToCmdPtyBlob(ctx, cmd.SessionId, cmd.CmdId, nil, 0)
	if err != nil {
		return nil, err
	}
	msh.AddRunningCmd(startPk.CK)
	return cmd, nil
}

func (msh *MShellProc) AddRunningCmd(ck base.CommandKey) {
	msh.Lock.Lock()
	defer msh.Lock.Unlock()
	msh.RunningCmds = append(msh.RunningCmds, ck)
}

func (msh *MShellProc) PacketRpc(ctx context.Context, pk packet.RpcPacketType) (*packet.ResponsePacketType, error) {
	if !msh.IsConnected() {
		return nil, fmt.Errorf("runner is not connected")
	}
	if pk == nil {
		return nil, fmt.Errorf("PacketRpc passed nil packet")
	}
	reqId := pk.GetReqId()
	msh.ServerProc.Output.RegisterRpc(reqId)
	defer msh.ServerProc.Output.UnRegisterRpc(reqId)
	err := msh.ServerProc.Input.SendPacketCtx(ctx, pk)
	if err != nil {
		return nil, err
	}
	rtnPk := msh.ServerProc.Output.WaitForResponse(ctx, reqId)
	if rtnPk == nil {
		return nil, ctx.Err()
	}
	if respPk, ok := rtnPk.(*packet.ResponsePacketType); ok {
		return respPk, nil
	}
	return nil, fmt.Errorf("invalid response packet received: %s", packet.AsString(rtnPk))
}

func (runner *MShellProc) WithLock(fn func()) {
	runner.Lock.Lock()
	defer runner.Lock.Unlock()
	fn()
}

func makeDataAckPacket(ck base.CommandKey, fdNum int, ackLen int, err error) *packet.DataAckPacketType {
	ack := packet.MakeDataAckPacket()
	ack.CK = ck
	ack.FdNum = fdNum
	ack.AckLen = ackLen
	if err != nil {
		ack.Error = err.Error()
	}
	return ack
}

func (msh *MShellProc) handleCmdDonePacket(donePk *packet.CmdDonePacketType) {
	err := sstore.UpdateCmdDonePk(context.Background(), donePk)
	if err != nil {
		fmt.Printf("[error] updating cmddone: %v\n", err)
		return
	}
	if GlobalStore.CmdStatusCallback != nil {
		GlobalStore.CmdStatusCallback(donePk.CK, sstore.CmdStatusDone)
	}
	return
}

func (msh *MShellProc) handleCmdErrorPacket(errPk *packet.CmdErrorPacketType) {
	err := sstore.AppendCmdErrorPk(context.Background(), errPk)
	if err != nil {
		fmt.Printf("[error] adding cmderr: %v\n", err)
		return
	}
	return
}

func (msh *MShellProc) notifyHangups_nolock() {
	if GlobalStore.CmdStatusCallback != nil {
		for _, ck := range msh.RunningCmds {
			GlobalStore.CmdStatusCallback(ck, sstore.CmdStatusHangup)
		}
	}
	msh.RunningCmds = nil
}

func (runner *MShellProc) ProcessPackets() {
	defer runner.WithLock(func() {
		if runner.Status == StatusConnected {
			runner.Status = StatusDisconnected
		}
		err := sstore.HangupRunningCmdsByRemoteId(context.Background(), runner.Remote.RemoteId)
		if err != nil {
			fmt.Printf("[error] calling HUP on remoteid=%d cmds\n", runner.Remote.RemoteId)
		}
		runner.notifyHangups_nolock()
	})
	dataPosMap := make(map[base.CommandKey]int64)
	for pk := range runner.ServerProc.Output.MainCh {
		if pk.GetType() == packet.DataPacketStr {
			dataPk := pk.(*packet.DataPacketType)
			realData, err := base64.StdEncoding.DecodeString(dataPk.Data64)
			if err != nil {
				ack := makeDataAckPacket(dataPk.CK, dataPk.FdNum, 0, err)
				runner.ServerProc.Input.SendPacket(ack)
				continue
			}
			var ack *packet.DataAckPacketType
			if len(realData) > 0 {
				dataPos := dataPosMap[dataPk.CK]
				err = sstore.AppendToCmdPtyBlob(context.Background(), dataPk.CK.GetSessionId(), dataPk.CK.GetCmdId(), realData, dataPos)
				if err != nil {
					ack = makeDataAckPacket(dataPk.CK, dataPk.FdNum, 0, err)
				} else {
					ack = makeDataAckPacket(dataPk.CK, dataPk.FdNum, len(realData), nil)
				}
				dataPosMap[dataPk.CK] += int64(len(realData))
			}
			if ack != nil {
				runner.ServerProc.Input.SendPacket(ack)
			}
			// fmt.Printf("data %s fd=%d len=%d eof=%v err=%v\n", dataPk.CK, dataPk.FdNum, len(realData), dataPk.Eof, dataPk.Error)
			continue
		}
		if pk.GetType() == packet.CmdDataPacketStr {
			dataPacket := pk.(*packet.CmdDataPacketType)
			fmt.Printf("cmd-data %s pty=%d run=%d\n", dataPacket.CK, dataPacket.PtyDataLen, dataPacket.RunDataLen)
			continue
		}
		if pk.GetType() == packet.CmdDonePacketStr {
			runner.handleCmdDonePacket(pk.(*packet.CmdDonePacketType))
			continue
		}
		if pk.GetType() == packet.CmdErrorPacketStr {
			runner.handleCmdErrorPacket(pk.(*packet.CmdErrorPacketType))
			continue
		}
		if pk.GetType() == packet.MessagePacketStr {
			msgPacket := pk.(*packet.MessagePacketType)
			fmt.Printf("# %s\n", msgPacket.Message)
			continue
		}
		if pk.GetType() == packet.RawPacketStr {
			rawPacket := pk.(*packet.RawPacketType)
			fmt.Printf("stderr> %s\n", rawPacket.Data)
			continue
		}
		if pk.GetType() == packet.CmdStartPacketStr {
			startPk := pk.(*packet.CmdStartPacketType)
			fmt.Printf("start> reqid=%s (%p)\n", startPk.RespId, runner.ServerProc.Output)
			continue
		}
		fmt.Printf("MSH> %s\n", packet.AsString(pk))
	}
}
