package core

import (
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/ricochet-im/ricochet-go/core/utils"
	"github.com/ricochet-im/ricochet-go/rpc"
	protocol "github.com/s-rah/go-ricochet"
	channels "github.com/s-rah/go-ricochet/channels"
	connection "github.com/s-rah/go-ricochet/connection"
	"golang.org/x/net/context"
	"log"
	"sync"
	"time"
)

type Contact struct {
	core *Ricochet

	data *ricochet.Contact

	mutex  sync.Mutex
	events *utils.Publisher

	connEnabled       bool
	connection        *connection.Connection
	connChannel       chan *connection.Connection
	connEnabledSignal chan bool
	connectionOnce    sync.Once

	timeConnected time.Time

	conversation *Conversation
}

func ContactFromConfig(core *Ricochet, data *ricochet.Contact, events *utils.Publisher) (*Contact, error) {
	contact := &Contact{
		core:              core,
		data:              data,
		events:            events,
		connChannel:       make(chan *connection.Connection),
		connEnabledSignal: make(chan bool),
	}

	if !IsAddressValid(data.Address) {
		return nil, fmt.Errorf("Invalid contact address '%s", data.Address)
	}

	if data.Request != nil {
		if data.Request.Rejected {
			contact.data.Status = ricochet.Contact_REJECTED
		} else {
			contact.data.Status = ricochet.Contact_REQUEST
		}
	} else if contact.data.Status != ricochet.Contact_REJECTED {
		contact.data.Status = ricochet.Contact_UNKNOWN
	}

	return contact, nil
}

func (c *Contact) Nickname() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.data.Nickname
}

func (c *Contact) Address() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.data.Address
}

func (c *Contact) Hostname() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	hostname, _ := OnionFromAddress(c.data.Address)
	return hostname
}

func (c *Contact) LastConnected() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	time, _ := time.Parse(time.RFC3339, c.data.LastConnected)
	return time
}

func (c *Contact) WhenCreated() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	time, _ := time.Parse(time.RFC3339, c.data.WhenCreated)
	return time
}

func (c *Contact) Status() ricochet.Contact_Status {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.data.Status
}

func (c *Contact) Data() *ricochet.Contact {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return proto.Clone(c.data).(*ricochet.Contact)
}

func (c *Contact) IsRequest() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.data.Request != nil
}

func (c *Contact) Conversation() *Conversation {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.conversation == nil {
		entity := &ricochet.Entity{
			Address: c.data.Address,
		}
		c.conversation = NewConversation(c, entity, c.core.Identity.ConversationStream)
	}
	return c.conversation
}

func (c *Contact) Connection() *connection.Connection {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.connection
}

// StartConnection enables inbound and outbound connections for this contact, if other
// conditions permit them. This function is safe to call repeatedly.
func (c *Contact) StartConnection() {
	c.connectionOnce.Do(func() {
		go c.contactConnection()
	})

	c.connEnabled = true
	c.connEnabledSignal <- true
}

func (c *Contact) StopConnection() {
	// Must be running to consume connEnabledSignal
	c.connectionOnce.Do(func() {
		go c.contactConnection()
	})

	c.connEnabled = false
	c.connEnabledSignal <- false
}

func (c *Contact) shouldMakeOutboundConnections() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Don't make connections to contacts in the REJECTED state
	if c.data.Status == ricochet.Contact_REJECTED {
		return false
	}

	return c.connEnabled
}

// closeUnhandledConnection takes a connection without an active Process routine
// and ensures that it is fully closed and destroyed. It is safe to call on
// a connection that has already been closed and on any connection in any
// state, as long as Process() is not currently running.
func closeUnhandledConnection(conn *connection.Connection) {
	conn.Conn.Close()
	nullHandler := &connection.AutoConnectionHandler{}
	nullHandler.Init()
	conn.Process(nullHandler)
}

// Goroutine to handle the protocol connection for a contact.
// Responsible for making outbound connections and taking over authenticated
// inbound connections, running protocol handlers on the active connection, and
// reacting to connection loss. Nothing else may write Contact.connection.
//
// This goroutine is started by the first call to StartConnection or StopConnection
// and persists for the lifetime of the contact. When connections are stopped, it
// consumes connChannel and closes all (presumably inbound) connections.
// XXX Need a hard kill for destroying contacts
func (c *Contact) contactConnection() {
	// Signalled when the active connection is closed
	connClosedChannel := make(chan struct{})
	connectionsEnabled := false

	for {
		if !connectionsEnabled {
			// Reject all connections on connChannel and wait for start signal
			select {
			case conn := <-c.connChannel:
				if conn != nil {
					log.Printf("Discarded connection to %s because connections are disabled", c.Address())
					go closeUnhandledConnection(conn)
					// XXX-protocol doing this here instead of during auth means they'll keep trying endlessly. Doing it in
					// auth means they'll never try again. Both are sometimes wrong. Hmm.
				}
			case enable := <-c.connEnabledSignal:
				if enable {
					log.Printf("Contact %s connections are enabled", c.Address())
					connectionsEnabled = true
				}
				// XXX hard kill
			}
			continue
		}

		// If there is no active connection, spawn an outbound connector. A successful connection
		// is returned via connChannel, and otherwise it will keep trying until cancelled via
		// the context.
		var outboundCtx context.Context
		outboundCancel := func() {}
		if c.connection == nil && c.shouldMakeOutboundConnections() {
			outboundCtx, outboundCancel = context.WithCancel(context.Background())
			go c.connectOutbound(outboundCtx, c.connChannel)
		}

		select {
		case conn := <-c.connChannel:
			outboundCancel()
			if conn == nil {
				// Signal used to restart outbound connection attempts
				continue
			}

			c.mutex.Lock()
			// Decide whether to keep this connection; if this returns an error, conn is
			// already closed. If there was an existing connection and this returns nil,
			// the old connection is closed but c.connection has not been reset.
			if err := c.considerUsingConnection(conn); err != nil {
				log.Printf("Discarded new contact %s connection: %s", c.data.Address, err)
				go closeUnhandledConnection(conn)
				c.mutex.Unlock()
				continue
			}
			replacingConn := c.connection != nil
			c.connection = conn
			if replacingConn {
				// Wait for old handleConnection to return
				c.mutex.Unlock()
				<-connClosedChannel
				c.mutex.Lock()
			}
			go c.handleConnection(conn, connClosedChannel)
			c.onConnectionStateChanged()
			c.mutex.Unlock()

		case <-connClosedChannel:
			outboundCancel()
			c.mutex.Lock()
			c.connection = nil
			c.onConnectionStateChanged()
			c.mutex.Unlock()

		case enable := <-c.connEnabledSignal:
			outboundCancel()
			if !enable {
				connectionsEnabled = false
				log.Printf("Contact %s connections are disabled", c.Address())
			}
		}
	}

	log.Printf("Exiting contact connection loop for %s", c.Address())
	c.mutex.Lock()
	if c.connection != nil {
		c.connection.Conn.Close()
		c.connection = nil
		c.onConnectionStateChanged()
		c.mutex.Unlock()
		<-connClosedChannel
	} else {
		c.mutex.Unlock()
	}
}

// Goroutine to maintain an open contact connection, calls Process and reports when closed.
func (c *Contact) handleConnection(conn *connection.Connection, closedChannel chan struct{}) {
	// Connection does not outlive this function
	defer func() {
		conn.Conn.Close()
		closedChannel <- struct{}{}
	}()
	log.Printf("Contact connection for %s ready", conn.RemoteHostname)
	handler := NewContactProtocolHandler(c, conn)
	err := conn.Process(handler)
	if err == nil {
		// Somebody called Break?
		err = fmt.Errorf("Connection handler interrupted unexpectedly")
	}
	log.Printf("Contact connection for %s closed: %s", conn.RemoteHostname, err)
}

// Attempt an outbound connection to the contact, retrying automatically using OnionConnector.
// This function _must_ send something to connChannel before returning, unless the context has
// been cancelled.
func (c *Contact) connectOutbound(ctx context.Context, connChannel chan *connection.Connection) {
	c.mutex.Lock()
	connector := OnionConnector{
		Network:     c.core.Network,
		NeverGiveUp: true,
	}
	hostname, _ := OnionFromAddress(c.data.Address)
	isRequest := c.data.Request != nil
	c.mutex.Unlock()

	for {
		conn, err := connector.Connect(hostname+":9878", ctx)
		if err != nil {
			// The only failure here should be context, because NeverGiveUp
			// is set, but be robust anyway.
			if ctx.Err() != nil {
				return
			}

			log.Printf("Contact connection failure: %s", err)
			continue
		}

		// XXX-protocol Ideally this should all take place under ctx also; easy option is a goroutine
		// blocked on ctx that kills the connection.
		log.Printf("Successful outbound connection to contact %s", hostname)
		oc, err := protocol.NegotiateVersionOutbound(conn, hostname[0:16])
		if err != nil {
			log.Printf("Outbound connection version negotiation failed: %v", err)
			conn.Close()
			if err := connector.Backoff(ctx); err != nil {
				return
			}
			continue
		}

		log.Printf("Outbound connection negotiated version; authenticating")
		privateKey := c.core.Identity.PrivateKey()
		known, err := connection.HandleOutboundConnection(oc).ProcessAuthAsClient(&privateKey)
		if err != nil {
			log.Printf("Outbound connection authentication failed: %v", err)
			closeUnhandledConnection(oc)
			if err := connector.Backoff(ctx); err != nil {
				return
			}
			continue
		}

		if !known && !isRequest {
			log.Printf("Outbound connection to contact says we are not a known contact for %v", c)
			// XXX Should move to rejected status, stop attempting connections.
			closeUnhandledConnection(oc)
			if err := connector.Backoff(ctx); err != nil {
				return
			}
			continue
		} else if known && isRequest {
			log.Printf("Contact request implicitly accepted for outbound connection by contact %v", c)
			c.UpdateContactRequest("Accepted")
			isRequest = false
		}

		if isRequest {
			// Need to send a contact request; this will block until the peer accepts or rejects,
			// the connection fails, or the context is cancelled (which also closes the connection).
			if err := c.sendContactRequest(oc, ctx); err != nil {
				log.Printf("Outbound contact request connection closed: %s", err)
				if err := connector.Backoff(ctx); err != nil {
					return
				}
				continue
			} else {
				log.Printf("Outbound contact request accepted, assigning connection")
			}
		}

		log.Printf("Assigning outbound connection to contact")
		c.AssignConnection(oc)
		break
	}
}

type requestChannelHandler struct {
	Response chan string
}

func (r *requestChannelHandler) ContactRequest(name, message string) string {
	log.Printf("BUG: inbound ContactRequest handler called for outbound channel")
	return "Error"
}
func (r *requestChannelHandler) ContactRequestRejected() { r.Response <- "Rejected" }
func (r *requestChannelHandler) ContactRequestAccepted() { r.Response <- "Accepted" }
func (r *requestChannelHandler) ContactRequestError()    { r.Response <- "Error" }

// sendContactRequest synchronously delivers a contact request to an authenticated
// outbound connection and waits for a final (yes/no) reply. This may be cancelled
// by closing the connection. Once a reply is received, it's passed to
// UpdateContactRequest to update the status and this function will return. nil is
// returned for an accepted request when the connection is still established. In all
// other cases, an error is returned and the connection will be closed.
func (c *Contact) sendContactRequest(conn *connection.Connection, ctx context.Context) error {
	log.Printf("Sending request to outbound contact %v", c)
	ach := &connection.AutoConnectionHandler{}
	ach.Init()

	processChan := make(chan error)
	responseChan := make(chan string)

	// No timeouts on outbound contact request; wait forever for a final reply
	go func() {
		processChan <- conn.Process(ach)
	}()

	err := conn.Do(func() error {
		_, err := conn.RequestOpenChannel("im.ricochet.contact.request",
			&channels.ContactRequestChannel{
				Handler: &requestChannelHandler{Response: responseChan},
				Name:    c.data.Request.FromNickname, // XXX mutex
				Message: c.data.Request.Text,
			})
		return err
	})
	if err != nil {
		// Close and end Process, resulting in an error to processChan and return when done
		conn.Conn.Close()
		return <-processChan
	}

	select {
	case err := <-processChan:
		// Should not get nil (via Break) return values here; prevent them
		if err == nil {
			closeUnhandledConnection(conn)
			err = fmt.Errorf("unknown connection break")
		}
		return err

	case response := <-responseChan:
		c.UpdateContactRequest(response)
		if response == "Accepted" {
			conn.Break()
			return <-processChan // nil if connection is still alive
		} else {
			conn.Conn.Close()
			return <-processChan
		}

	case <-ctx.Done():
		conn.Conn.Close()
		return <-processChan
	}
}

// considerUsingConnection takes a newly established connection and decides whether
// the new connection is valid and acceptable, and whether to replace or keep an
// existing connection. To handle race cases when peers are connecting to eachother,
// a particular set of rules is followed for replacing an existing connection.
//
// considerUsingConnection returns nil if the new connection is valid and should be
// used. If this function returns nil, the existing connection has been closed (but
// c.connection is unmodified, and the process routine may still be executing). If
// this function returns an error, conn has been closed.
//
// Assumes that c.mutex is held.
func (c *Contact) considerUsingConnection(conn *connection.Connection) error {
	killConn := conn
	defer func() {
		if killConn != nil {
			killConn.Conn.Close()
		}
	}()

	if conn.IsInbound {
		log.Printf("Contact %s has a new inbound connection", c.data.Address)
	} else {
		log.Printf("Contact %s has a new outbound connection", c.data.Address)
	}

	if conn == c.connection {
		return fmt.Errorf("Duplicate assignment of connection %v to contact %v", conn, c)
	}

	if !conn.Authentication["im.ricochet.auth.hidden-service"] {
		return fmt.Errorf("Connection %v is not authenticated", conn)
	}

	plainHost, _ := PlainHostFromAddress(c.data.Address)
	if plainHost != conn.RemoteHostname {
		return fmt.Errorf("Connection hostname %s doesn't match contact hostname %s when assigning connection", conn.RemoteHostname, plainHost)
	}

	if c.connection != nil && !c.shouldReplaceConnection(conn) {
		return fmt.Errorf("Using existing connection")
	}

	// If this connection is inbound and there's an outbound attempt, keep this
	// connection and cancel outbound if we haven't sent authentication yet, or
	// if the outbound connection will lose the fallback comparison above.
	// XXX implement this; currently outbound is always cancelled when an inbound
	// connection succeeds.

	// We will keep conn, close c.connection instead if there was one
	killConn = c.connection
	return nil
}

// onConnectionStateChanged is called by the connection loop when the c.connection
// is changed, which can be a transition to online or offline or a replacement.
// Assumes c.mutex is held.
func (c *Contact) onConnectionStateChanged() {
	if c.connection != nil {
		if c.data.Request != nil && c.connection.IsInbound {
			// Inbound connection implicitly accepts the contact request and can continue as a contact
			// Outbound request logic is all handled by connectOutbound.
			log.Printf("Contact request implicitly accepted by contact %v", c)
			c.updateContactRequest("Accepted")
		} else {
			c.data.Status = ricochet.Contact_ONLINE
		}
	} else {
		if c.data.Status == ricochet.Contact_ONLINE {
			c.data.Status = ricochet.Contact_OFFLINE
		}
	}

	// Update LastConnected time
	c.timeConnected = time.Now()
	c.data.LastConnected = c.timeConnected.Format(time.RFC3339)

	config := c.core.Config.Lock()
	config.Contacts[c.data.Address] = c.data
	c.core.Config.Unlock()

	// XXX I wonder if events and config updates can be combined now, and made safer...
	// _really_ assumes c.mutex was held
	c.mutex.Unlock()
	event := ricochet.ContactEvent{
		Type: ricochet.ContactEvent_UPDATE,
		Subject: &ricochet.ContactEvent_Contact{
			Contact: c.Data(),
		},
	}
	c.events.Publish(event)

	if c.connection != nil {
		// Send any queued messages
		sent := c.Conversation().SendQueuedMessages()
		if sent > 0 {
			log.Printf("Sent %d queued messages to contact", sent)
		}
	}

	c.mutex.Lock()
}

// Decide whether to replace the existing connection with conn.
// Assumes mutex is held.
func (c *Contact) shouldReplaceConnection(conn *connection.Connection) bool {
	myHostname, _ := PlainHostFromAddress(c.core.Identity.Address())
	if c.connection == nil {
		return true
	} else if c.connection.IsInbound == conn.IsInbound {
		// If the existing connection is in the same direction, always use the new one
		log.Printf("Replacing existing same-direction connection %v with new connection %v for contact %v", c.connection, conn, c)
		return true
	} else if time.Since(c.timeConnected) > (30 * time.Second) {
		// If the existing connection is more than 30 seconds old, use the new one
		log.Printf("Replacing existing %v old connection %v with new connection %v for contact %v", time.Since(c.timeConnected), c.connection, conn, c)
		return true
	} else if preferOutbound := myHostname < conn.RemoteHostname; preferOutbound != conn.IsInbound {
		// Fall back to string comparison of hostnames for a stable resolution
		// New connection wins
		log.Printf("Replacing existing connection %v with new connection %v for contact %v according to fallback order", c.connection, conn, c)
		return true
	} else {
		// Old connection wins fallback
		log.Printf("Keeping existing connection %v instead of new connection %v for contact %v according to fallback order", c.connection, conn, c)
		return false
	}
	return false
}

// Update the status of a contact request from a protocol event. Returns
// true if the contact request channel should remain open.
func (c *Contact) UpdateContactRequest(status string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.data.Request == nil {
		return false
	}

	re := c.updateContactRequest(status)

	event := ricochet.ContactEvent{
		Type: ricochet.ContactEvent_UPDATE,
		Subject: &ricochet.ContactEvent_Contact{
			Contact: c.Data(),
		},
	}
	c.events.Publish(event)

	return re
}

// Same as above, but assumes the mutex is already held and that the caller
// will send an UPDATE event
func (c *Contact) updateContactRequest(status string) bool {
	now := time.Now().Format(time.RFC3339)
	// Whether to keep the channel open
	var re bool

	switch status {
	case "Pending":
		c.data.Request.WhenDelivered = now
		re = true

	case "Accepted":
		c.data.Request = nil
		if c.connection != nil {
			c.data.Status = ricochet.Contact_ONLINE
		} else {
			c.data.Status = ricochet.Contact_UNKNOWN
		}

	case "Rejected":
		c.data.Request.WhenRejected = now

	case "Error":
		c.data.Request.WhenRejected = now
		c.data.Request.RemoteError = "error occurred"

	default:
		log.Printf("Unknown contact request status '%s'", status)
	}

	config := c.core.Config.Lock()
	defer c.core.Config.Unlock()
	config.Contacts[c.data.Address] = c.data
	return re
}

// AssignConnection takes new connections, inbound or outbound, to this contact, and
// asynchronously decides whether to keep or close them.
func (c *Contact) AssignConnection(conn *connection.Connection) {
	c.connectionOnce.Do(func() {
		go c.contactConnection()
	})

	// If connections are disabled, this connection will be closed by contactConnection
	c.connChannel <- conn
}
