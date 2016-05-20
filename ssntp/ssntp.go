//
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package ssntp

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/docker/distribution/uuid"
	"github.com/golang/glog"
)

// Type is the SSNTP frame type.
// It can be COMMAND, STATUS, ERROR or EVENT.
type Type uint8

// Command is the SSNTP Command operand.
// It can be CONNECT, START, STOP, STATS, EVACUATE, DELETE, RESTART,
// AssignPublicIP, ReleasePublicIP or CONFIGURE.
type Command uint8

// Status is the SSNTP Status operand.
// It can be CONNECTED, READY, FULL, OFFLINE or MAINTENANCE
type Status uint8

// Role describes the SSNTP role for the frame sender.
// It can be UNKNOWN, SERVER, Controller, AGENT, SCHEDULER, NETAGENT or CNCIAGENT.
type Role uint32

// Error is the SSNTP Error operand.
// It can be InvalidFrameType Error, StartFailure,
// StopFailure, ConnectionFailure, RestartFailure,
// DeleteFailure, ConnectionAborted or InvalidConfiguration.
type Error uint8

// Event is the SSNTP Event operand.
// It can be TenantAdded, TenantRemoval, InstanceDeleted,
// ConcentratorInstanceAdded, PublicIPAssigned, TraceReport,
// NodeConnected or NodeDisconnected
type Event uint8

const (
	// COMMAND frames are meant for SSNTP clients to send commands.
	// For example the Controller sends START or STOP commands to launch and
	// pause workloads.
	// SSNTP being asynchronous SSNTP commands are not replied to.
	COMMAND Type = iota

	// STATUS frames are mostly used by the launcher agent to report
	// about the node status. It is used by the scheduler as an indication
	// for its next scheduling decisions. Status frames can be seen as
	// a way of building flow control between the scheduler and the launchers.
	STATUS

	// ERROR frames contain error reports. Combining the error operand together
	// with the Error frame YAML payload allows for building a complete error
	// interpretation and description.
	// ERROR frames are typically sent for command failures.
	ERROR

	// EVENT frames carry asynchronous events that the receiver can decide to
	// broadcast or not.
	// EVENT frames describe a general, non erratic cluster event.
	EVENT
)

const (
	// CONNECT is the first frame sent by an SSNTP client to establish the SSNTP
	// connection. A server will ignore any clients until it sends its first CONNECT
	// frame:
	//					   SSNTP CONNECT Command frame
	//
	//	+-------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |          Role             |
	//	|       |       | (0x0) |  (0x0)  | (bitmask of client roles) |
	//	+-------------------------------------------------------------+
	CONNECT Command = iota

	// START is a command that should reach CIAO agents for scheduling a new
	// on the compute node (CN) they manage. It should typically come from the Controller
	// entity directly or via the main server:
	//					   SSNTP START Command frame
	//
	//	+-----------------------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted workload description |
	//	|       |       | (0x0) |  (0x1)  |                 |                                     |
	//	+-----------------------------------------------------------------------------------------+
	START

	// STOP is used to ask a CIAO agent to stop a running workload. The workload
	// is identified by its UUID, as part of the YAML formatted payload:
	//					   SSNTP STOP Command frame
	//
	//	+----------------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted workload UUID |
	//	|       |       | (0x0) |  (0x2)  |                 |                              |
	//	+----------------------------------------------------------------------------------+
	STOP

	// STATS is a command sent by CIAO agents to update the SSNTP network
	// about their compute node statistics. Agents can send that command to either
	// the main server or to the Controllers directly. In the former case the server will
	// be responsible for forwarding it to the known Controllers.
	// The conpute node statistics form the YAML formatted payload for this command:
	//					   SSNTP STATS Command frame
	//
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted compute |
	//	|       |       | (0x0) |  (0x3)  |                 | node statistics        |
	//	+----------------------------------------------------------------------------+
	STATS

	// EVACUATE is intended to ask a specific CIAO agent to evacuate its compute
	// node, i.e. stop and migrate all of the current workloads he's monitoring on
	// this node. The payload for this command is a YAML formatted description of the
	// next state to reach after evacuation is done. It could be 'shutdown' for shutting
	// the node down, 'update' for having it run a software update, 'reboot' for rebooting
	// the node or 'maintenance' for putting the node in maintenance mode:
	//	+---------------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted compute      |
	//	|       |       | (0x0) |  (0x4)  |                 | node next state description |
	//	+---------------------------------------------------------------------------------+
	EVACUATE

	// DELETE is a command sent to a CIAO CN Agent in order to completely delete a
	// running instance. This is only relevant for persistent workloads after they were
	// STOPPED. Non persistent workload get deleted when they are STOPPED.
	// It is up to the CN Agent implementation to decide what exactly needs to be deleted
	// on the CN but a deleted instance will no longer be able to boot.
	// The DELETE command payload uses the same YAML schema as the STOP command one, i.e.
	// an instance UUID and an agent UUID.
	//                                         SSNTP DELETE Command frame
	//	+------------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload   |
	//	|       |       | (0x0) |  (0x5)  |                 | instance and agent UUIDs |
	//	+------------------------------------------------------------------------------+
	DELETE

	// RESTART is a command sent to CIAO CN Agents for restarting an instance that was
	// previously STOPped. This command is only relevant for persistent workloads since
	// non persistent ones are implicitly deleted when STOPped and thus can not be
	// RESTARTed.
	// The RESTART command payload uses the same YAML schema as the STOP command one, i.e.
	// an instance UUID and an agent UUID.
	//                                         SSNTP DELETE Command frame
	//	+------------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload   |
	//	|       |       | (0x0) |  (0x6)  |                 | instance and agent UUIDs |
	//	+------------------------------------------------------------------------------+
	RESTART

	// AssignPublicIP is a command sent by the Controller to assign
	// a publicly routable IP to a given instance. It is sent
	// to the Scheduler and must be forwarded to the right CNCI.
	//
	// The public IP is fetched from a pre-allocated pool
	// managed by the Controller.
	//
	// The AssignPublicIP YAML payload schema is made of the
	// CNCI and a tenant UUIDs, the allocated public IP, the
	// instance private IP and MAC.
	//
	//                                         SSNTP AssignPublicIP Command frame
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload |
	//	|       |       | (0x0) |  (0x7)  |                 |                        |
	//	+----------------------------------------------------------------------------+
	AssignPublicIP

	// ReleasePublicIP is a command sent by the Controller to release
	// a publicly routable IP from a given instance. It is sent
	// to the Scheduler and must be forwarded to the right CNCI.
	//
	// The released public IP is added back to the Controller managed
	// IP pool.
	//
	// The ReleasePublicIP YAML payload schema is made of the
	// CNCI and a tenant UUIDs, the released public IP, the
	// instance private IP and MAC.
	//
	//                                       SSNTP ReleasePublicIP Command frame
	//	+--------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted |
	//	|       |       | (0x0) |  (0x8)  |                 |     payload    |
	//	+--------------------------------------------------------------------+
	ReleasePublicIP

	// CONFIGURE commands are sent to request any SSNTP entity to
	// configure itself according to the CONFIGURE command payload.
	// Controller or any SSNTP client handling user interfaces defining any
	// cloud setting (image service, networking configuration, identity
	// management...) must send this command for any configuration
	// change and for broadcasting the initial cloud configuration to
	// all CN and NN agents.
	//
	// The CONFIGURE command payload always include the full cloud
	// configuration and not only changes compared to the last CONFIGURE
	// command sent.
	//
	//                                       SSNTP CONFIGURE Command frame
	//	+-----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload  |
	//	|       |       | (0x0) |  (0x9)  |                 |                         |
	//	+-----------------------------------------------------------------------------+
	CONFIGURE
)

const (
	// CONNECTED is the reply to a client CONNECT command and thus only SSNTP servers can
	// send such frame. The CONNECTED status confirms the client that it's connected and
	// that it should be prepared to process and send commands and statuses.
	// The CONNECTED payload contains the cloud configuration data. Please refer to the
	// CONFIGURE command frame for more details.
	//
	//					 SSNTP CONNECTED Status frame
	//
	//	+--------------------------------------------------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |         Role              | Server UUID | Client UUID | Payload | YAML formatted |
	//	|       |       | (0x1) |  (0x0)  | (bitmask of server roles) |             |             |  Length |      payload   |
	//	+--------------------------------------------------------------------------------------------------------------------+
	CONNECTED Status = iota

	// READY is a status command CIAO agents send to the scheduler to notify them about
	// their readiness to launch some more work (Virtual machines, containers or bare metal
	// ones). It is the only way for an agent to notify the CIAO scheduler about its
	// compute node capacity change and thus its readiness to take some more work. The new
	// CN capacity is described in this frame's payload:
	//					 SSNTP READY Status frame
	//
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted compute |
	//	|       |       | (0x1) |  (0x1)  |                 | node new capacity      |
	//	+----------------------------------------------------------------------------+
	READY

	// FULL is a status command CIAO agents send to the scheduler to let it know that
	// the compute node they control is now running at full capacity, i.e. it can temporarily
	// not run any additional work. The scheduler should stop sending START commands to such
	// agent until it receives a new READY status with some available capacity from it.
	//					 SSNTP FULL Status frame
	//
	//	+---------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length |
	//	|       |       | (0x1) |  (0x2)  |       (0x0)     |
	//	+---------------------------------------------------+
	FULL

	// OFFLINE is used by agents to let everyone know that although they're still running
	// and connected to the SSNTP network they are not ready to receive any kind of command,
	// be it START, STOP or EVACUATE ones.
	//
	//					 SSNTP OFFLINE Status frame
	//
	//	+---------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length |
	//	|       |       | (0x1) |  (0x3)  |       (0x0)     |
	//	+---------------------------------------------------+
	OFFLINE

	// MAINTENANCE is used by agents to let the scheduler know that it entered maintenance
	// mode.
	//
	//					 SSNTP MAINTENANCE Status frame
	//
	//	+---------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length |
	//	|       |       | (0x1) |  (0x4)  |       (0x0)     |
	//	+---------------------------------------------------+
	MAINTENANCE
)

const (
	// TenantAdded is used by workload agents to notify networking agents that the first
	// workload for a given tenant has just started. Networking agents need to know about that
	// so that they can forward it to the right CNCI (Compute Node Concentrator Instance), i.e.
	// the CNCI running the tenant workload.
	//					 SSNTP TenantAdded Event frame
	//
	//	+---------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted tenant |
	//	|       |       | (0x3) |  (0x0)  |                 | information           |
	//	+---------------------------------------------------------------------------+
	TenantAdded Event = iota

	// TenantRemoved is used by workload agents to notify networking agents that the last
	// workload for a given tenant has just terminated. Networking agents need to know about that
	// so that they can forward it to the right CNCI (Compute Node Concentrator Instance), i.e.
	// the CNCI running the tenant workload.
	//					 SSNTP TenantRemoved Event frame
	//
	//	+--------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted tenant |
	//	|       |       | (0x3) |  (0x1)  |                 | information           |
	//	+---------------------------------------------------------------------------+
	TenantRemoved

	// InstanceDeleted is sent by workload agents to notify the scheduler and the Controller that a
	// previously running instance has been deleted. While the scheduler and the Controller could infer
	// that information from the next STATS command (The deleted instance would no longer be there)
	// it is safer, simpler and less error prone to explicitly send this event.
	//
	//					 SSNTP InstanceDeleted Event frame
	//
	//	+---------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted        |
	//	|       |       | (0x3) |  (0x2)  |                 | instance information  |
	//	+---------------------------------------------------------------------------+
	InstanceDeleted

	// ConcentratorInstanceAdded events are sent by networking nodes
	// agents to the Scheduler in order to notify the SSNTP network
	// that a networking concentrator instance (CNCI) is now running
	// on this node.
	// A CNCI handles the GRE tunnel concentrator for a given
	// tenant. Each instance started by this tenant will have a
	// GRE tunnel established between it and the CNCI allowing all
	// instances for a given tenant to be on the same private
	// network.
	//
	// The Scheduler must forward that event to all Controllers. The Controller
	// needs to know about it as it will fetch the CNCI IP and the
	// tenant UUID from this event's payload and pass that through
	// the START payload when scheduling a new instance for this
	// tenant. A tenant instances can not be scheduled until Controller gets
	// a ConcentratorInstanceAdded event as instances will be
	// isolated as long as the CNCI for this tenant is not running.
	//
	//					 SSNTP ConcentratorInstanceAdded Event frame
	//
	//	+--------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted       |
	//	|       |       | (0x3) |  (0x3)  |                 | CNCI information     |
	//	+--------------------------------------------------------------------------+
	ConcentratorInstanceAdded

	// PublicIPAssigned events are sent by Networking concentrator
	// instances (CNCI) to the Scheduler when they successfully
	// assigned a public IP to a given instance.
	// The public IP can either come from a Controller pre-allocated pool,
	// or from a control network DHCP server.
	//
	// The Scheduler must forward those events to the Controller.
	//
	// The PublicIPAssigned event payload contains the newly assigned
	// public IP, the instance private IP and the instance UUID.
	//
	//					 SSNTP PublicIPAssigned Event frame
	//
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload |
	//	|       |       | (0x3) |  (0x4)  |                 |                        |
	//	+----------------------------------------------------------------------------+
	PublicIPAssigned

	// TraceReport events carry a tracing report payload from one
	// of the SSNTP clients.
	//
	//					 SSNTP TraceReport Event frame
	//
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload |
	//	|       |       | (0x3) |  (0x5)  |                 |                        |
	//	+----------------------------------------------------------------------------+
	TraceReport

	// NodeConnected events are sent by the Scheduler to notify e.g. the Controllers about
	// a new compute or networking node being connected.
	// The NodeConnected event payload contains the connected node UUID and the node type
	// (compute or networking)
	//
	//					 SSNTP NodeConnected Event frame
	//
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload |
	//	|       |       | (0x3) |  (0x6)  |                 |                        |
	//	+----------------------------------------------------------------------------+
	NodeConnected

	// NodeDisconnected events are sent by the Scheduler to notify e.g. the Controllers about
	// a new compute or networking node disconnection.
	// The NodeDisconnected event payload contains the discconnected node UUID and the node
	// type (compute or networking)
	//
	//					 SSNTP NodeDisconnected Event frame
	//
	//	+----------------------------------------------------------------------------+
	//	| Major | Minor | Type  | Operand |  Payload Length | YAML formatted payload |
	//	|       |       | (0x3) |  (0x7)  |                 |                        |
	//	+----------------------------------------------------------------------------+
	NodeDisconnected
)

// SSNTP clients and servers can have one or several roles and are expected to declare their
// roles during the SSNTP connection procedure.
const (
	UNKNOWN Role = 0x0
	SERVER       = 0x1

	// The Command and Status Reporter. This is a client role.
	Controller = 0x2

	// The cloud compute node agent. This is a client role.
	AGENT = 0x4

	// The workload scheduler. This is a server role.
	SCHEDULER = 0x8

	// The networking compute node agent. This is a client role.
	NETAGENT = 0x10

	// The networking compute node concentrator instance (CNCI) agent. This is a client role.
	CNCIAGENT = 0x20
)

// We use SSL extended key usage attributes for specifying and verifying SSNTP
// client and server claimed roles.
// For example if a client claims to be a Controller, then its client certificate
// extended key usage attribute should contain the right OID for that role.
var (
	// RoleAgentOID is the SSNTP Agent Role Object ID.
	RoleAgentOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 343, 8, 1}

	// RoleSchedulerOID is the SSNTP Scheduler Role Object ID.
	RoleSchedulerOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 343, 8, 2}

	// RoleControllerOID is the SSNTP Controller Role Object ID.
	RoleControllerOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 343, 8, 3}

	// RoleNetAgentOID is the SSNTP Networking Agent Role Object ID.
	RoleNetAgentOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 343, 8, 4}

	// RoleAgentOID is the SSNTP Server Role Object ID.
	RoleServerOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 343, 8, 5}

	// RoleCNCIAgentOID is the SSNTP Compute Node Concentrator Instance Agent Role Object ID.
	RoleCNCIAgentOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 343, 8, 6}
)

const (
	// InvalidFrameType is sent when receiving an unsupported frame type.
	InvalidFrameType Error = iota

	// StartFailure is sent by launcher agents to report a workload start failure.
	StartFailure

	// StopFailure is sent by launcher agents to report a workload pause failure.
	StopFailure

	// ConnectionFailure is sent to report an SSNTP connection failure.
	// It can be sent by servers and clients.
	ConnectionFailure

	// RestartFailure is sent by launcher agents to report a workload re-start failure.
	RestartFailure

	// DeleteFailure is sent by launcher agents to report a workload deletion failure.
	DeleteFailure

	// ConnectionAborted is sent to report an SSNTP connection abortion.
	// This is used for example when receiving bad certificates.
	ConnectionAborted

	// InvalidConfiguration is either sent by the Scheduler to report an invalid
	// CONFIGURE payload back to the sender, or by the clients to which a CONFIGURE
	// command has been forwarded to and that leads to configuration errors on their
	// side.
	// When the scheduler receives such error back from any client it should revert
	// back to the previous valid configuration.
	InvalidConfiguration
)

const major = 0
const minor = 1
const defaultURL = "localhost"
const port = 8888
const readTimeout = 30
const writeTimeout = 30

const defaultCA = "/etc/pki/ciao/ca_cert.crt"
const defaultServerCert = "/etc/pki/ciao/server.pem"
const defaultClientCert = "/etc/pki/ciao/client.pem"
const uuidPrefix = "/var/lib/ciao/local/uuid-storage/role"
const uuidLockPrefix = "/tmp/lock/ciao"

func (t Type) String() string {
	switch t {
	case COMMAND:
		return "COMMAND"
	case STATUS:
		return "STATUS"
	case EVENT:
		return "EVENT"
	case ERROR:
		return "ERROR"
	}

	return ""
}

func (command Command) String() string {
	switch command {
	case CONNECT:
		return "CONNECT"
	case START:
		return "START"
	case STOP:
		return "STOP"
	case STATS:
		return "STATISTICS"
	case EVACUATE:
		return "EVACUATE"
	case DELETE:
		return "DELETE"
	case RESTART:
		return "RESTART"
	case AssignPublicIP:
		return "Assign public IP"
	case ReleasePublicIP:
		return "Release public IP"
	case CONFIGURE:
		return "CONFIGURE"
	}

	return ""
}

func (status Status) String() string {
	switch status {
	case CONNECTED:
		return "CONNECTED"
	case READY:
		return "READY"
	case FULL:
		return "FULL"
	case OFFLINE:
		return "OFFLINE"
	case MAINTENANCE:
		return "MAINTENANCE"
	}

	return ""
}

func (status Event) String() string {
	switch status {
	case TenantAdded:
		return "Tenant Added"
	case TenantRemoved:
		return "Tenant Removed"
	case InstanceDeleted:
		return "Instance Deleted"
	case ConcentratorInstanceAdded:
		return "Network Concentrator Instance Added"
	case PublicIPAssigned:
		return "Public IP Assigned"
	case TraceReport:
		return "Trace Report"
	case NodeConnected:
		return "Node Connected"
	case NodeDisconnected:
		return "Node Disconnected"
	}

	return ""
}

func (error Error) String() string {
	switch error {
	case InvalidFrameType:
		return "Invalid SSNTP frame type"
	case StartFailure:
		return "Could not start instance"
	case StopFailure:
		return "Could not stop instance"
	case ConnectionFailure:
		return "SSNTP Connection failed"
	case RestartFailure:
		return "Could not restart instance"
	case DeleteFailure:
		return "Could not delete instance"
	case ConnectionAborted:
		return "SSNTP Connection aborted"
	case InvalidConfiguration:
		return "Cluster configuration is invalid"
	}

	return ""
}

func (role *Role) String() string {
	roleString := ""

	if *role&SERVER == SERVER {
		roleString += "Server-"
	}

	if *role&Controller == Controller {
		roleString += "Controller-"
	}

	if *role&AGENT == AGENT {
		roleString += "CNAgent-"
	}

	if *role&SCHEDULER == SCHEDULER {
		roleString += "Scheduler-"
	}

	if *role&NETAGENT == NETAGENT {
		roleString += "NetworkingAgent-"
	}

	if *role&CNCIAGENT == CNCIAGENT {
		roleString += "CNCIAgent-"
	}

	return roleString
}

// Set sets an SSNTP role based on the input string.
func (role *Role) Set(value string) error {
	for _, r := range strings.Split(value, ",") {
		if r == "unknown" {
			*role |= UNKNOWN
		} else if r == "server" {
			*role |= SERVER
		} else if r == "controller" {
			*role |= Controller
		} else if r == "agent" {
			*role |= AGENT
		} else if r == "netagent" {
			*role |= NETAGENT
		} else if r == "scheduler" {
			*role |= SCHEDULER
		} else if r == "cnciagent" {
			*role |= CNCIAGENT
		} else {
			return errors.New("Unknown role")
		}
	}

	return nil
}

// A Config structure is used to configure a SSNTP client or server.
// It is mandatory to provide an SSNTP configuration when starting
// an SSNTP server or when connecting to one as a client.
type Config struct {
	// UUID is the client or server UUID string. If set to "",
	// the SSNTP package will generate a random one.
	UUID string

	// URI semantic differs between servers and clients.
	// For clients it represents the the SSNTP server URI
	// they want to connect to.
	// For servers it represents the URI they will be
	// listening on.
	// When set to "" SSNTP servers will listen on all interfaces
	// and IPs on the running host.
	URI string

	// Role is a bitmask of SSNTP roles the client or server intends
	// to run.
	Role uint32

	// CACert is the Certification Authority certificate path
	// to use when verifiying the peer identity.
	// If set to "", /etc/pki/ciao/ciao_ca_cert.crt will be used.
	CAcert string

	// Cert is the client or server x509 signed certificate path.
	// If set to "", /etc/pki/ciao/client.pem and /etc/pki/ciao/ciao.pem
	// will be used for SSNTP clients and server, respectively.
	Cert string

	// Transport is the underlying transport protocol. Only "tcp" and "unix"
	// transports are supported. The default is "tcp".
	Transport string

	// ForwardRules is optional and contains a list of frame forwarding rules.
	ForwardRules []FrameForwardRule

	// Log is the SSNTP logging interface.
	// If not set, only error messages will be logged.
	// The SSNTP Log implementation provides a default logger.
	Log Logger

	// When RoleVerification is true the peer declared role will be
	// verified by checking that the received certificate extended
	// key usage attributes contains the right OID.
	RoleVerification bool

	// TCP port to connect (Client) or to listen to (Server).
	// This is optional, the default SSNTP port is 8888.
	Port uint32

	// Trace configures the desired level of SSNTP frame tracing.
	Trace *TraceConfig
}

// Logger is an interface for SSNTP users to define their own
// SSNTP tracing routines.
// By default we use errLog and we also provide Log, a glog based
// SSNTPLogger implementation.
type Logger interface {
	Errorf(format string, args ...interface{})
	Warningf(format string, args ...interface{})
	Infof(format string, args ...interface{})
}

type errorLog struct{}

func (l errorLog) Errorf(format string, args ...interface{}) {
	log.Printf("SSNTP Error: "+format, args...)
}

func (l errorLog) Warningf(format string, args ...interface{}) {
}

func (l errorLog) Infof(format string, args ...interface{}) {
}

var errLog errorLog

type glogLog struct{}

func (l glogLog) Infof(format string, args ...interface{}) {
	if glog.V(2) {
		glog.Infof("SSNTP Info: "+format, args...)
	}
}

func (l glogLog) Errorf(format string, args ...interface{}) {
	glog.Errorf("SSNTP Error: "+format, args...)
}

func (l glogLog) Warningf(format string, args ...interface{}) {
	if glog.V(1) {
		glog.Warningf("SSNTP Warning: "+format, args...)
	}
}

// Log is a glog based SSNTP Logger implementation.
// Error message will be logged unconditionally.
// Warnings are logged if glog's V >= 1.
// Info messages are logged if glog's V >= 2.
var Log glogLog

type boolFlag struct {
	sync.Mutex
	flag bool
}

type ssntpStatus uint32

const (
	ssntpIdle ssntpStatus = iota
	ssntpConnecting
	ssntpConnected
	ssntpClosed
)

type connectionStatus struct {
	sync.Mutex
	status ssntpStatus
}

type clusterConfiguration struct {
	sync.RWMutex
	configuration []byte
}

func (conf *clusterConfiguration) setConfiguration(configuration []byte) {
	conf.Lock()
	conf.configuration = configuration
	conf.Unlock()
}

func prepareTLSConfig(config *Config, server bool) *tls.Config {
	caPEM, err := ioutil.ReadFile(config.CAcert)
	if err != nil {
		log.Fatalf("SSNTP: Load CA certificate: %s", err)
	}

	certPEM, err := ioutil.ReadFile(config.Cert)
	if err != nil {
		log.Fatalf("SSNTP: Load Certificate: %s", err)
	}

	return prepareTLS(caPEM, certPEM, server)
}

func prepareTLS(caPEM, certPEM []byte, server bool) *tls.Config {
	cert, err := tls.X509KeyPair(certPEM, certPEM)
	if err != nil {
		log.Printf("SSNTP: Load Key: %s", err)
		return nil
	}

	certPool := x509.NewCertPool()
	if certPool.AppendCertsFromPEM(caPEM) != true {
		log.Print("SSNTP: Could not append CA")
		return nil
	}

	if server == true {
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      certPool,
			ClientCAs:    certPool,
			Rand:         rand.Reader,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      certPool,
	}
}

var roleOID = []struct {
	role uint32
	oid  asn1.ObjectIdentifier
}{
	{
		role: AGENT,
		oid:  RoleAgentOID,
	},
	{
		role: SCHEDULER,
		oid:  RoleSchedulerOID,
	},
	{
		role: Controller,
		oid:  RoleControllerOID,
	},
	{
		role: NETAGENT,
		oid:  RoleNetAgentOID,
	},
	{
		role: SERVER,
		oid:  RoleServerOID,
	},
	{
		role: CNCIAGENT,
		oid:  RoleCNCIAgentOID,
	},
}

func getRoleFromOIDs(oids []asn1.ObjectIdentifier) uint32 {
	role := (uint32)(UNKNOWN)

	for _, oid := range oids {
		for _, r := range roleOID {
			if r.oid.Equal(oid) {
				role |= r.role
			}
		}
	}

	return role
}

func getOIDsFromRole(role uint32) ([]asn1.ObjectIdentifier, error) {
	var oids []asn1.ObjectIdentifier
	for _, r := range roleOID {
		if role&r.role == r.role {
			oids = append(oids, r.oid)
		}
	}

	if len(oids) == 0 {
		return nil, fmt.Errorf("Unknown role 0x%x", role)
	}

	return oids, nil
}

func verifyRole(conn interface{}, role uint32) (bool, error) {
	var oidError = fmt.Errorf("**** TEMPORARY WARNING ****\n*** Wrong certificate or missing/mismatched role OID ***\nIn order to fix this, use the -role option when generating your certificates with the ciao-cert tool.\n")
	switch tlsConn := conn.(type) {
	case *tls.Conn:
		state := tlsConn.ConnectionState()
		certRole := getRoleFromOIDs(state.PeerCertificates[0].UnknownExtKeyUsage)
		if certRole&role != role {
			return false, oidError
		}

		return true, nil
	}

	return false, oidError
}

func parseCertificateAuthority(config *Config) ([]string, []string, error) {
	var fqdns []string
	var ips []string
	caPEM, err := ioutil.ReadFile(config.CAcert)
	if err != nil {
		log.Fatalf("SSNTP: Load CA certificate: %s", err)
	}

	certBlock, _ := pem.Decode(caPEM)
	if certBlock == nil {
		fmt.Printf("Could not decode PEM for %s\n", config.CAcert)
	}

	cert, err := x509.ParseCertificates(certBlock.Bytes)
	if err != nil {
		fmt.Printf("Could not parse certificate %s\n", err)
		return nil, nil, err
	}

	for _, fqdn := range cert[0].DNSNames {
		fqdns = append(fqdns, fqdn)
	}

	for _, ip := range cert[0].IPAddresses {
		ips = append(ips, ip.String())
	}

	return ips, fqdns, nil
}

func parseCertificate(config *Config) (uint32, error) {
	certPEM, err := ioutil.ReadFile(config.Cert)
	if err != nil {
		log.Fatalf("SSNTP: Load certificate: %s", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		fmt.Printf("Could not decode PEM for %s\n", config.CAcert)
	}

	cert, err := x509.ParseCertificates(certBlock.Bytes)
	if err != nil {
		fmt.Printf("Could not parse certificate %s\n", err)
		return 0, err
	}

	role := getRoleFromOIDs(cert[0].UnknownExtKeyUsage)
	/* We could not find a valid OID in the certificate */
	if role == (uint32)(UNKNOWN) {
		return role, fmt.Errorf("Could not find a SSNTP role")
	}

	return role, nil
}

const nullUUID = "00000000-0000-0000-0000-000000000000"

type lockedUUID struct {
	lockFd int
	uuid   uuid.UUID
}

func newUUID(prefix string, role uint32) (lockedUUID, error) {
	uuidFile := fmt.Sprintf("%s/%s/0x%x", uuidPrefix, prefix, role)
	uuidLockFile := fmt.Sprintf("%s/%s-role-0x%x", uuidLockPrefix, prefix, role)
	_nUUID, _ := uuid.Parse(nullUUID)
	nUUID := lockedUUID{
		uuid:   _nUUID,
		lockFd: -1,
	}

	randomUUID := lockedUUID{
		uuid:   uuid.Generate(),
		lockFd: -1,
	}

	/* Create UUID directory if necessary */
	err := os.MkdirAll(uuidPrefix+"/"+prefix, 0755)
	if err != nil {
		fmt.Printf("Unable to create %s %v\n", uuidPrefix, err)
	}

	/* Create CIAO lock directory if necessary */
	err = os.MkdirAll(uuidLockPrefix, 0777)
	if err != nil {
		fmt.Printf("Unable to create %s %v\n", uuidLockPrefix, err)
		return nUUID, err
	}

	fd, err := syscall.Open(uuidFile, syscall.O_CREAT|syscall.O_RDWR, syscall.S_IWUSR|syscall.S_IRUSR)
	if err != nil {
		fmt.Printf("Unable to open UUID file %s %v\n", uuidFile, err)
		return nUUID, err
	}

	defer func() { _ = syscall.Close(fd) }()

	lockFd, err := syscall.Open(uuidLockFile, syscall.O_CREAT, syscall.S_IWUSR|syscall.S_IRUSR)
	if err != nil {
		fmt.Printf("Unable to open UUID lock file %s %v\n", uuidLockFile, err)
		return nUUID, err
	}

	if syscall.Flock(lockFd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		/* File is already locked, we need to generate a random UUID */
		syscall.Close(lockFd)
		return randomUUID, nil
	}

	uuidArray := make([]byte, 36)
	n, err := syscall.Read(fd, uuidArray)
	if err != nil {
		fmt.Printf("Could not read %s\n", uuidFile)
		syscall.Close(lockFd)
		return nUUID, err
	}

	if n == 0 || n != 36 {
		/* 2 cases: */
		/* 1) File was just created or is empty: Write a new UUID */
		/* Or */
		/* 2) File contains garbage - Overwrite with a new UUID */
		newUUID := uuid.Generate()
		_, err := syscall.Write(fd, []byte(newUUID.String()))
		if err != nil {
			fmt.Printf("Could not write %s on %s (%s)\n", newUUID.String(), uuidFile, err)
			syscall.Close(lockFd)
			return nUUID, err
		}

		newLockedUUID := lockedUUID{
			uuid:   newUUID,
			lockFd: lockFd,
		}

		return newLockedUUID, nil
	} else if n == 36 {
		newUUID, err := uuid.Parse(string(uuidArray[:36]))
		if err != nil {
			fmt.Printf("Could not parse UUID\n")
			syscall.Close(lockFd)
			return nUUID, err
		}

		newLockedUUID := lockedUUID{
			uuid:   newUUID,
			lockFd: lockFd,
		}

		return newLockedUUID, nil
	}

	return nUUID, err
}

func freeUUID(uuid lockedUUID) error {
	if uuid.lockFd == -1 {
		return nil
	}

	err := syscall.Flock(uuid.lockFd, syscall.LOCK_UN)
	if err != nil {
		fmt.Printf("Unable to unlock UUID %v\n", err)
		return err
	}

	return nil
}
