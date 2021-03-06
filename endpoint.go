package serversets

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/samuel/go-zookeeper/zk"
)

// An Endpoint is a service (host and port) registered on Zookeeper
// to be discovered by clients/watchers.
type Endpoint struct {
	*ServerSet
	PingRate   time.Duration // default 1 second
	CloseEvent chan struct{}

	disconnect chan struct{}
	done       chan struct{}

	host string
	port int

	key    string
	ping   func() error
	alive  bool
	closed chan struct{} // kills the ping function

	once sync.Once // to only close once
}

// RegisterEndpoint registers a host and port as alive. It creates the appropriate
// Zookeeper nodes and watchers will be notified this server/endpoint is available.
func (ss *ServerSet) RegisterEndpoint(host string, port int, ping func() error) (*Endpoint, error) {
	endpoint := &Endpoint{
		ServerSet:  ss,
		PingRate:   time.Second,
		CloseEvent: make(chan struct{}, 1),
		disconnect: make(chan struct{}),
		done:       make(chan struct{}),
		host:       host,
		port:       port,
		ping:       ping,
		alive:      true,
		closed:     make(chan struct{}, 1),
	}

	if ping != nil {
		endpoint.alive = endpoint.ping() == nil
	}

	connection, sessionEvents, err := ss.connectToZookeeper()
	if err != nil {
		return nil, err
	}

	err = endpoint.update(connection)
	if err != nil {
		return nil, err
	}

	// spawn goroutine to deal with connection/session issues.
	go func() {
		for {
			select {
			case event := <-sessionEvents:
				if event.Type == zk.EventSession && event.State == zk.StateExpired {
					connection.Close()
					connection = nil
				}
			case <-endpoint.disconnect:
				endpoint.closed <- struct{}{}
				connection.Close()
				endpoint.done <- struct{}{}
				return
			}

			if connection == nil {
				connection, sessionEvents, err = ss.connectToZookeeper()
				if err != nil {
					panic(fmt.Errorf("unable to reconnect to zookeeper after session expired: %v", err))
				}

				err = endpoint.update(connection)
				if err != nil {
					panic(fmt.Errorf("unable to reregister endpoint after session expired: %v", err))
				}
			}
		}
	}()

	if ping != nil {
		go func() {
			for {
				time.Sleep(endpoint.PingRate)

				select {
				case <-endpoint.closed:
					return
				default:
					alive := endpoint.ping() == nil
					if alive != endpoint.alive {
						endpoint.alive = alive
						err := endpoint.update(connection)

						if err != nil {
							panic(fmt.Errorf("unable to reregister after ping change: %v", err))
						}
					}
				}
			}
		}()
	}

	return endpoint, nil
}

// Close blocks until the client connection to Zookeeper is closed.
// If already called, will simply return, even if in the process of closing.
func (ep *Endpoint) Close() {
	ep.once.Do(func() {
		ep.disconnect <- struct{}{}
		<-ep.done
		ep.CloseEvent <- struct{}{}
	})

	return
}

func (ep *Endpoint) update(connection *zk.Conn) error {
	// don't create/remove the node if we're dead
	if !ep.alive {
		if ep.key != "" {
			err := connection.Delete(ep.key, 0)
			ep.key = ""
			return err
		}

		return nil
	}

	entityData, _ := json.Marshal(newEntity(ep.host, ep.port))

	var err error
	ep.key, err = ep.ServerSet.registerEndpoint(connection, entityData)

	return err
}

func (ss *ServerSet) registerEndpoint(connection *zk.Conn, data []byte) (string, error) {
	err := ss.createFullPath(connection)
	if err != nil {
		return "", err
	}

	return connection.Create(
		ss.directoryPath()+"/"+MemberPrefix,
		data,
		zk.FlagEphemeral|zk.FlagSequence,
		zk.WorldACL(zk.PermAll))
}
