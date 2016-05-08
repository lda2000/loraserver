package loraserver

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/brocaar/loraserver/models"
	"github.com/brocaar/lorawan"
	"github.com/garyburd/redigo/redis"
	"github.com/jmoiron/sqlx"
)

// NodeTXPayloadQueueTTL defines the TTL of the node TXPayload queue
var NodeTXPayloadQueueTTL = time.Hour * 24 * 5

// errEmptyQueue defines the error returned when the queue is empty.
// Note that depending the context, this error might not be a real error.
var errEmptyQueue = errors.New("the queue is empty or does not exist")

const (
	nodeTXPayloadQueueTempl     = "node_tx_queue_%s"
	nodeTXPayloadInProcessTempl = "node_tx_in_process_%s"
)

// createNode creates the given Node.
func createNode(db *sqlx.DB, n models.Node) error {
	if n.RXDelay > 15 {
		return errors.New("max value of RXDelay is 15")
	}

	_, err := db.Exec("insert into node (dev_eui, app_eui, app_key, rx_delay) values ($1, $2, $3, $4)",
		n.DevEUI[:],
		n.AppEUI[:],
		n.AppKey[:],
		n.RXDelay,
	)
	if err != nil {
		return fmt.Errorf("create node %s error: %s", n.DevEUI, err)
	}
	log.WithField("dev_eui", n.DevEUI).Info("node created")
	return nil
}

// updateNode updates the given Node.
func updateNode(db *sqlx.DB, n models.Node) error {
	if n.RXDelay > 15 {
		return errors.New("max value of RXDelay is 15")
	}

	res, err := db.Exec("update node set app_eui = $1, app_key = $2, used_dev_nonces = $3, rx_delay = $4 where dev_eui = $5",
		n.AppEUI[:],
		n.AppKey[:],
		n.UsedDevNonces,
		n.RXDelay,
		n.DevEUI[:],
	)
	if err != nil {
		return fmt.Errorf("update node %s error: %s", n.DevEUI, err)
	}
	ra, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if ra == 0 {
		return fmt.Errorf("node %s does not exist", n.DevEUI)
	}
	log.WithField("dev_eui", n.DevEUI).Info("node updated")
	return nil
}

// deleteNode deletes the Node matching the given DevEUI.
func deleteNode(db *sqlx.DB, devEUI lorawan.EUI64) error {
	res, err := db.Exec("delete from node where dev_eui = $1",
		devEUI[:],
	)
	if err != nil {
		return fmt.Errorf("delete node %s error: %s", devEUI, err)
	}
	ra, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if ra == 0 {
		return fmt.Errorf("node %s does not exist", devEUI)
	}
	log.WithField("dev_eui", devEUI).Info("node deleted")
	return nil
}

// getNode returns the Node for the given DevEUI.
func getNode(db *sqlx.DB, devEUI lorawan.EUI64) (models.Node, error) {
	var node models.Node
	err := db.Get(&node, "select * from node where dev_eui = $1", devEUI[:])
	if err != nil {
		return node, fmt.Errorf("get node %s error: %s", devEUI, err)
	}
	return node, nil
}

// getNodes returns a slice of nodes, sorted by DevEUI.
func getNodes(db *sqlx.DB, limit, offset int) ([]models.Node, error) {
	var nodes []models.Node
	err := db.Select(&nodes, "select * from node order by dev_eui limit $1 offset $2", limit, offset)
	if err != nil {
		return nodes, fmt.Errorf("get nodes error: %s", err)
	}
	return nodes, nil
}

// addTXPayloadToQueue adds the given TXPayload to the queue.
func addTXPayloadToQueue(p *redis.Pool, payload models.TXPayload) error {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encode tx payload for node %s error: %s", payload.DevEUI, err)
	}

	c := p.Get()
	defer c.Close()

	exp := int64(NodeTXPayloadQueueTTL) / int64(time.Millisecond)
	key := fmt.Sprintf(nodeTXPayloadQueueTempl, payload.DevEUI)

	c.Send("MULTI")
	c.Send("LPUSH", key, buf.Bytes())
	c.Send("PEXPIRE", key, exp)
	_, err := c.Do("EXEC")

	if err != nil {
		return fmt.Errorf("add tx payload to queue for node %s error: %s", payload.DevEUI, err)
	}

	log.WithField("dev_eui", payload.DevEUI).Info("tx payload added to queue")
	return nil
}

// getTXPayloadAndRemainingFromQueue returns the first TXPayload to send
// to the node and a bool indicating if there are more payloads pending to be
// send. This is either an in-process item (e.g. an item that needs
// to be re-transmitted or an item from the queue (which will be marked as
// in-process when consuming). After a successful transmission, don't forget
// to call clearInProcessTXPayload.
// nil is returned when there are no items in the queue.
func getTXPayloadAndRemainingFromQueue(p *redis.Pool, devEUI lorawan.EUI64) (*models.TXPayload, bool, error) {
	// in-process queue
	inProcessTXPayload, remaining, err := getTXPayloadAndRemainingFromInProcess(p, devEUI)
	if err != nil {
		return nil, false, fmt.Errorf("get tx payload from in-process queue error: %s", err)
	}
	// TXPayload in process or nothing remaining in the queue
	if inProcessTXPayload != nil || !remaining {
		return inProcessTXPayload, remaining, nil
	}

	var txPayload models.TXPayload
	queueKey := fmt.Sprintf(nodeTXPayloadQueueTempl, devEUI)
	inProcessKey := fmt.Sprintf(nodeTXPayloadInProcessTempl, devEUI)

	c := p.Get()
	defer c.Close()

	c.Send("RPOPLPUSH", queueKey, inProcessKey)
	c.Send("LLEN", queueKey)
	c.Flush()

	// read bytes
	b, err := redis.Bytes(c.Receive())
	if err != nil {
		if err == redis.ErrNil {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get tx payload from queue for node %s error: %s", devEUI, err)
	}

	// read remaining
	i, err := redis.Int(c.Receive())
	if err != nil {
		return nil, false, fmt.Errorf("read remaining tx payload items in queue for node %s error: %s", devEUI, err)
	}

	if err = gob.NewDecoder(bytes.NewReader(b)).Decode(&txPayload); err != nil {
		return nil, false, fmt.Errorf("decode tx payload for node %s error: %s", devEUI, err)
	}
	return &txPayload, i > 0, nil
}

func getTXPayloadAndRemainingFromInProcess(p *redis.Pool, devEUI lorawan.EUI64) (*models.TXPayload, bool, error) {
	var txPayload models.TXPayload
	queueKey := fmt.Sprintf(nodeTXPayloadQueueTempl, devEUI)
	inProcessKey := fmt.Sprintf(nodeTXPayloadInProcessTempl, devEUI)

	c := p.Get()
	defer c.Close()

	c.Send("LINDEX", inProcessKey, -1)
	c.Send("LLEN", queueKey)
	c.Flush()

	b, err := redis.Bytes(c.Receive())
	if err != nil {
		// nothing in-process, just return the queue size
		if err == redis.ErrNil {
			i, err := redis.Int(c.Receive())
			if err != nil {
				return nil, false, fmt.Errorf("read node %s queue size error: %s", devEUI, err)
			}
			return nil, i > 0, nil
		}
		return nil, false, fmt.Errorf("get tx payload from in-process queue for node %s error: %s", devEUI, err)
	}

	i, err := redis.Int(c.Receive())
	if err != nil {
		return nil, false, fmt.Errorf("read node %s queue size error: %s", devEUI, err)
	}

	if err = gob.NewDecoder(bytes.NewReader(b)).Decode(&txPayload); err != nil {
		return nil, false, fmt.Errorf("decode tx payload for node %s error: %s", devEUI, err)
	}

	return &txPayload, i > 0, nil
}

// clearInProcessTXPayload clears the in-process TXPayload (to be called
// after a successful transmission). It returns the TXPayload or nil when
// nothing was cleared (it already expired).
func clearInProcessTXPayload(p *redis.Pool, devEUI lorawan.EUI64) (*models.TXPayload, error) {
	var txPayload models.TXPayload
	key := fmt.Sprintf(nodeTXPayloadInProcessTempl, devEUI)
	c := p.Get()
	defer c.Close()
	b, err := redis.Bytes(c.Do("RPOP", key))
	if err != nil {
		if err == redis.ErrNil {
			return nil, nil
		}
		return nil, fmt.Errorf("clear in-process tx payload for node %s failed: %s", devEUI, err)
	}
	log.WithField("dev_eui", devEUI).Info("in-process tx payload removed")
	err = gob.NewDecoder(bytes.NewReader(b)).Decode(&txPayload)
	if err != nil {
		return nil, fmt.Errorf("decode tx payload for node %s error: %s", devEUI, err)
	}
	return &txPayload, nil
}

// NodeAPI exports the Node related functions.
type NodeAPI struct {
	ctx Context
}

// NewNodeAPI creates a new NodeAPI.
func NewNodeAPI(ctx Context) *NodeAPI {
	return &NodeAPI{
		ctx: ctx,
	}
}

// Get returns the Node for the given DevEUI.
func (a *NodeAPI) Get(devEUI lorawan.EUI64, node *models.Node) error {
	var err error
	*node, err = getNode(a.ctx.DB, devEUI)
	return err
}

// GetList returns a list of nodes (given a limit and offset).
func (a *NodeAPI) GetList(req models.GetListRequest, nodes *[]models.Node) error {
	var err error
	*nodes, err = getNodes(a.ctx.DB, req.Limit, req.Offset)
	return err
}

// Create creates the given Node.
func (a *NodeAPI) Create(node models.Node, devEUI *lorawan.EUI64) error {
	if err := createNode(a.ctx.DB, node); err != nil {
		return err
	}
	*devEUI = node.DevEUI
	return nil
}

// Update updatest the given Node.
func (a *NodeAPI) Update(node models.Node, devEUI *lorawan.EUI64) error {
	if err := updateNode(a.ctx.DB, node); err != nil {
		return err
	}
	*devEUI = node.DevEUI
	return nil
}

// Delete deletes the node matching the given DevEUI.
func (a *NodeAPI) Delete(devEUI lorawan.EUI64, deletedDevEUI *lorawan.EUI64) error {
	if err := deleteNode(a.ctx.DB, devEUI); err != nil {
		return err
	}
	*deletedDevEUI = devEUI
	return nil
}
