package mpvjson

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

//Connection is the connection object. Should be open through the entire lifecycle.
type Connection struct {
	Client       net.Conn
	lock         *sync.RWMutex
	usedIDs      []uint
	usedEventIDs []uint
	response     map[uint]chan Response
	event        map[uint]chan Response
}

//Command contains a request which will be marshalled and sent as a request to the unix socket.
type Command struct {
	RequestID uint          `json:"request_id"`
	Arguments []interface{} `json:"command"`
}

//Response is currently used for every response received from the socket. Might eventually separate the normal response object from the event object but for now I like it being one.
type Response struct {
	Data      interface{} `json:"data"`
	Success   string      `json:"error"`
	RequestID uint        `json:"request_id"`
	Event     string      `json:"event"`
	Name      string      `json:"name"`
	EventID   uint        `json:"id"`
}

//Open will open the connection to the unix socket to communicate with mpv and initialize some variables.
func Open(socket string) (Connection, error) {
	var err error
	var c Connection

	c.lock = &sync.RWMutex{}

	c.Client, err = net.Dial("unix", socket)
	if err != nil {
		return c, err
	}
	// defer c.Client.Close()
	c.response = make(map[uint]chan Response)
	c.event = make(map[uint]chan Response)
	go c.readInput()
	return c, nil

}

//Close will close everything to do with the connection.
func (c *Connection) Close() error {
	// log.Println("CLOSING")
	c.lock.Lock()
	toClose := c.usedEventIDs
	// log.Println(toClose)
	c.usedEventIDs = c.usedEventIDs[:0]
	c.lock.Unlock()
	for _, id := range toClose {
		err := c.unobserve(id)
		if err != nil {
			log.Println(err)
		}
		close(c.event[id])
	}
	err := c.Client.Close()
	if err != nil {
		return err
	}
	// log.Println("CLOSED")
	return nil
}

//Run is a function to run a manual command. Just need to pass in a list of arguments and it will call a function to marshal them in json and run the command.
func (c *Connection) Run(arguments ...interface{}) (Response, error) {
	response, err := c.exec(arguments)
	if err != nil {
		return response, err
	}
	return response, nil
}

//Set is used to set a property. example arguments would be ("pause", true)
func (c *Connection) Set(arguments ...interface{}) (Response, error) {
	args := make([]interface{}, len(arguments)+1)
	args[0] = "set_property"
	for i, argument := range arguments {
		args[i+1] = argument
	}
	response, err := c.exec(args)
	if err != nil {
		return response, err
	}
	return response, nil
}

//Get is used to get the current status of a property.
func (c *Connection) Get(arguments ...interface{}) (Response, error) {
	args := make([]interface{}, len(arguments)+1)
	args[0] = "get_property"
	for i, argument := range arguments {
		args[i+1] = argument
	}
	response, err := c.exec(args)
	if err != nil {
		return response, err
	}
	return response, nil
}

//Observe will watch a property and send a Response object through the responseC channel for every event it receives.
func (c *Connection) Observe(responseC chan Response, continueC chan bool, arguments ...interface{}) error {
	args := make([]interface{}, len(arguments)+2)
	args[0] = "observe_property"
	for i, argument := range arguments {
		args[i+2] = argument
	}
	//status channel is used to check if the status of observing the property in general was a success.
	statusC := make(chan Response)
	go c.watch(responseC, statusC, continueC, args)
	//After starting script to watch wait for the status to be returned
	status := <-statusC
	//if status wasn't successful send a false to the continue channel which stops the watching function and return that starting the observe command was unsuccessful.
	if status.Success != "success" {
		continueC <- false
		return fmt.Errorf("Failed to observe property - %s.", status.Success)
	}
	//otherwise send true so it can begin watching for events. and return no errors.
	// continueC <- true
	return nil
}

//unobserve is an internal function used to stop observing a property. It is called after continue channel receives a false.
func (c *Connection) unobserve(id uint) error {
	args := make([]interface{}, 2)
	args[0] = "unobserve_property"
	args[1] = id
	_, err := c.exec(args)
	if err != nil {
		return err
	}
	return nil
}

func (c *Connection) exec(arguments []interface{}) (Response, error) {
	var response Response

	//This will get the jsonified command string to be sent through mpv and the next id to be used.
	commandJSON, nextID, _, err := c.buildCommand(false, arguments)
	if err != nil {
		return response, err
		// log.Println(err)
	}

	//this will send the command
	err = c.writeCmd(commandJSON)
	if err != nil {
		return response, err
	}

	response = <-c.response[nextID]
	c.lock.Lock()
	// log.Printf("sent response to %d", nextID)
	// log.Printf("deleting index %d from usedIDs", nextID)
	c.usedIDs = deleteIndex(nextID, c.usedIDs)
	// log.Printf("closed index %d from usedIDs. Closing channel.", nextID)
	// close(c.response[nextID])
	// log.Printf("closed channel %d on response map", nextID)
	c.lock.Unlock()
	return response, nil

}

func (c *Connection) watch(responseC chan Response, statusC chan Response, continueC chan bool, arguments []interface{}) error {
	var response Response
	statusSent := false
	//This will get the jsonified command string to be sent through mpv and the next id to be used.
	commandJSON, nextID, eventID, err := c.buildCommand(true, arguments)
	if err != nil {
		return err
		// log.Println(err)
	}

	//this will send the command
	err = c.writeCmd(commandJSON)
	if err != nil {
		return err
		// log.Println(err)
	}

	for {
		if statusSent == false {
			select {
			case response = <-c.response[nextID]:
				// log.Printf("received watch status response for %d", nextID)
				if statusSent == false {
					statusC <- response
					c.lock.Lock()
					// log.Printf("deleting index %d from usedIDs", nextID)
					c.usedIDs = deleteIndex(nextID, c.usedIDs)
					// log.Printf("closed index %d from usedIDs. Closing channel.", nextID)
					// close(c.response[nextID])
					// log.Printf("closed channel %d on response map", nextID)
					c.lock.Unlock()
					statusSent = true
				}
			case <-continueC:
				c.lock.Lock()
				// log.Printf("deleting index %d from eventIDs", eventID)
				c.usedEventIDs = deleteIndex(eventID, c.usedEventIDs)
				// log.Printf("closed index %d from eventIDs. Closing channel.", eventID)
				close(c.event[eventID])
				// log.Printf("closed channel %d on event map", eventID)
				c.lock.Unlock()
				return nil
			}
		} else {
			select {
			case response = <-c.event[eventID]:
				// log.Printf("received event response for %d", eventID)
				responseC <- response
			case <-continueC:
				err = c.unobserve(response.EventID)
				if err != nil {
					log.Println(err)
				}
				c.lock.Lock()
				// log.Printf("deleting index %d from eventIDs", eventID)
				c.usedEventIDs = deleteIndex(eventID, c.usedEventIDs)
				// log.Printf("closed index %d from eventIDs. Closing channel.", eventID)
				close(c.event[eventID])
				// log.Printf("closed channel %d on event map", eventID)
				// close(c.response[nextID])
				c.lock.Unlock()
				return nil
			}
		}
	}

}

func (c *Connection) buildCommand(watching bool, arguments []interface{}) ([]byte, uint, uint, error) {
	var eventID uint
	c.lock.Lock()
	// log.Println("finding next id")
	nextID := findNextID(c.usedIDs)
	// log.Printf("using id %d - adding to list", nextID)
	c.usedIDs = append(c.usedIDs, nextID)
	// log.Printf("added %d to id list", nextID)
	if watching == true {
		// log.Println("finding next event id")
		eventID = findNextID(c.usedEventIDs)
		// log.Printf("using event id %d - adding to event list", eventID)
		c.usedEventIDs = append(c.usedEventIDs, eventID)
		// log.Printf("added %d to event id list", eventID)
		arguments[1] = eventID
	}
	c.lock.Unlock()
	command := Command{
		RequestID: nextID,
		Arguments: arguments,
	}
	// log.Println(command)
	commandJSON, err := json.Marshal(command)
	if err != nil {
		return commandJSON, nextID, eventID, err
	}
	c.lock.Lock()
	// log.Printf("creating channel %d for response channel", nextID)
	c.response[nextID] = make(chan Response)
	// log.Printf("built response channel %d", nextID)
	if watching == true {
		// log.Printf("creating channel %d for event channel", nextID)
		c.event[eventID] = make(chan Response)
	}
	c.lock.Unlock()
	return commandJSON, nextID, eventID, nil
}

func (c *Connection) writeCmd(commandJSON []byte) error {
	var err error
	_, err = c.Client.Write(commandJSON)
	if err != nil {
		return err
	}
	_, err = c.Client.Write([]byte("\n"))
	if err != nil {
		return err
	}
	return nil
}

func (c *Connection) readInput() error {
	scanner := bufio.NewScanner(c.Client)
	for scanner.Scan() {
		var res Response
		// log.Println(scanner.Text())
		err := json.Unmarshal(scanner.Bytes(), &res)
		if err != nil {
			// log.Print(err)
			return err
		}
		// log.Println(res)
		//TODO I want both these IDs to be the same variable. Not sure how to make request_id and id map to same variable.
		if res.EventID != 0 {
			// log.Printf("sending event to %d", res.EventID)
			c.lock.Lock()
			c.event[res.EventID] <- res
			c.lock.Unlock()
		} else if res.RequestID != 0 {
			// log.Printf("sending response to %d", res.RequestID)
			c.lock.Lock()
			c.response[res.RequestID] <- res
			close(c.response[res.RequestID])
			// log.Printf("closed response channel %d", res.RequestID)
			c.lock.Unlock()
		} else {
			continue
			// log.Println(res)
			// log.Printf("event had no id. %s", res.Name)
		}
	}
	c.Client.Close()
	// log.Println("closing channel")
	return nil
}

func deleteIndex(currentID uint, allIDs []uint) []uint {
	// log.Printf("clearing ID %d", currentID)
	// log.Println(allIDs)
	for i, id := range allIDs {
		if currentID == id {
			allIDs = append(allIDs[0:i], allIDs[i+1:]...)
			break
		}
	}
	// log.Println(allIDs)
	// log.Println("cleared ID")
	return allIDs
}

func findNextID(allIDs []uint) uint {
	//TODO will need to remove ID's that have finished executing.
	if len(allIDs) == 0 {
		return 1
	}
	return allIDs[len(allIDs)-1] + 1
}
