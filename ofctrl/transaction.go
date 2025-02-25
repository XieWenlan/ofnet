package ofctrl

// The work flow to use transaction for sending multiple Openflow messages should be:
// NewTransaction->Begin->AddFlow->Complete->Commit. Client could cancel the messages by calling Abort after the
// transaction is complete and not Commit.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"antrea.io/libOpenflow/openflow13"
	"antrea.io/libOpenflow/util"
	log "github.com/sirupsen/logrus"
)

type TransactionType uint16

func (t TransactionType) getValue() uint16 {
	return uint16(t)
}

const (
	Atomic  = TransactionType(openflow13.OFPBCT_ATOMIC)
	Ordered = TransactionType(openflow13.OFPBCT_ORDERED)
)

var uid uint32 = 1

type OpenFlowModMessage interface {
	resetXid(xid uint32) util.Message
}

type Transaction struct {
	ofSwitch       *OFSwitch
	ID             uint32
	flag           TransactionType
	closed         bool
	lock           sync.Mutex
	successAdd     map[uint32]bool
	controlReplyCh chan MessageResult
	controlIntCh   chan MessageResult
}

// NewTransaction creates a transaction on the switch. It will assign a bundle ID, and sets the bundle flags.
func (self *OFSwitch) NewTransaction(flag TransactionType) *Transaction {
	tx := new(Transaction)
	tx.ID = atomic.AddUint32(&uid, 1)
	if flag == 0 {
		tx.flag = Atomic
	} else {
		tx.flag = flag
	}
	tx.ofSwitch = self
	tx.controlReplyCh = make(chan MessageResult, 10)
	tx.controlIntCh = make(chan MessageResult, 1)
	return tx
}

func (tx *Transaction) getError(reply MessageResult) error {
	errType := reply.GetErrorType()
	errCode := reply.GetErrorCode()
	if errType == openflow13.ET_EXPERIMENTER && errCode >= openflow13.BEC_UNKNOWN && errCode <= openflow13.BEC_BUNDLE_IN_PROCESS {
		return openflow13.ParseBundleError(reply.GetErrorCode())
	}
	return fmt.Errorf("unsupported bundle error with type %d and code %d", errType, errCode)
}

func (tx *Transaction) sendControlRequest(xID uint32, msg util.Message) error {
	if err := tx.ofSwitch.Send(msg); err != nil {
		return err
	}

	select {
	case reply := <-tx.controlIntCh:
		if reply.IsSucceed() {
			return nil
		} else {
			return tx.getError(reply)
		}
	case <-time.After(messageTimeout):
		return fmt.Errorf("bundle reply is timeout")
	case <-tx.ofSwitch.ctx.Done():
		return fmt.Errorf("bundle reply is canceled because of disconnection from the Switch")
	}
}

func (tx *Transaction) newBundleControlMessage(msgType uint16) *openflow13.VendorHeader {
	bundleCtrl := &openflow13.BundleControl{
		BundleID: tx.ID,
		Type:     msgType,
		Flags:    tx.flag.getValue(),
	}
	message := openflow13.NewBundleControl(bundleCtrl)
	return message
}

func (tx *Transaction) createBundleAddMessage(mod OpenFlowModMessage) (*openflow13.VendorHeader, error) {
	bundleAdd := &openflow13.BundleAdd{
		BundleID: tx.ID,
		Flags:    tx.flag.getValue(),
	}
	message := openflow13.NewBundleAdd(bundleAdd)
	bundleAdd.Message = mod.resetXid(message.Header.Xid)
	return message, nil
}

func (tx *Transaction) createBundleAddFlowMessage(flowMod *openflow13.FlowMod) (*openflow13.VendorHeader, error) {
	bundleAdd := &openflow13.BundleAdd{
		BundleID: tx.ID,
		Flags:    tx.flag.getValue(),
	}
	message := openflow13.NewBundleAdd(bundleAdd)
	flowMod.Xid = message.Header.Xid
	bundleAdd.Message = flowMod
	return message, nil
}

func (tx *Transaction) listenReply() {
	for {
		select {
		case reply, ok := <-tx.controlReplyCh:
			if !ok { // controlReplyCh closed.
				return
			}
			switch reply.msgType {
			case BundleControlMessage:
				select {
				case tx.controlIntCh <- reply:
				case <-time.After(messageTimeout):
					log.Warningln("BundleControlMessage reply message accept timeout")
				}
			case BundleAddMessage:
				if !reply.succeed {
					func() {
						tx.lock.Lock()
						defer tx.lock.Unlock()
						// Remove failed add message from successAdd.
						delete(tx.successAdd, reply.xID)
					}()
				}
			}
		}
	}
}

// Begin opens a bundle configuration.
func (tx *Transaction) Begin() error {
	message := tx.newBundleControlMessage(openflow13.OFPBCT_OPEN_REQUEST)
	tx.ofSwitch.subscribeMessage(tx.ID, tx.controlReplyCh)
	tx.successAdd = make(map[uint32]bool)
	// Start a new goroutine to listen Bundle Control reply and error messages if received from OFSwitch.
	go tx.listenReply()

	err := tx.sendControlRequest(message.Header.Xid, message)
	if err != nil {
		tx.ofSwitch.unSubscribeMessage(tx.ID)
		close(tx.controlReplyCh)
		return err
	}
	return nil
}

func (tx *Transaction) AddFlow(flowMod *openflow13.FlowMod) error {
	message, err := tx.createBundleAddFlowMessage(flowMod)
	if err != nil {
		return err
	}
	tx.lock.Lock()
	tx.successAdd[message.Header.Xid] = true
	tx.lock.Unlock()
	return tx.ofSwitch.Send(message)
}

// AddMessage adds messages in the bundle.
func (tx *Transaction) AddMessage(modMessage OpenFlowModMessage) error {
	message, err := tx.createBundleAddMessage(modMessage)
	if err != nil {
		return err
	}
	tx.lock.Lock()
	tx.successAdd[message.Header.Xid] = true
	tx.lock.Unlock()
	return tx.ofSwitch.Send(message)
}

// Complete closes the bundle configuration. It returns the number of successful messages added in the bundle.
func (tx *Transaction) Complete() (int, error) {
	if !tx.closed {
		msg1 := tx.newBundleControlMessage(openflow13.OFPBCT_CLOSE_REQUEST)
		if err := tx.sendControlRequest(msg1.Header.Xid, msg1); err != nil {
			return 0, err
		}
		tx.closed = true
	}
	tx.lock.Lock()
	defer tx.lock.Unlock()
	return len(tx.successAdd), nil
}

// Commit commits the bundle configuration. If transaction is not closed, it sends OFPBCT_CLOSE_REQUEST in advance.
func (tx *Transaction) Commit() error {
	if !tx.closed {
		return fmt.Errorf("transaction %d is not complete", tx.ID)
	}
	defer func() {
		tx.ofSwitch.unSubscribeMessage(tx.ID)
		close(tx.controlReplyCh)
	}()
	msg := tx.newBundleControlMessage(openflow13.OFPBCT_COMMIT_REQUEST)
	if err := tx.sendControlRequest(msg.Header.Xid, msg); err != nil {
		return err
	}
	return nil
}

// Abort discards the bundle configuration. If transaction is not closed, it sends OFPBCT_CLOSE_REQUEST in advance.
func (tx *Transaction) Abort() error {
	if !tx.closed {
		return fmt.Errorf("transaction %d is not complete", tx.ID)
	}
	defer func() {
		tx.ofSwitch.unSubscribeMessage(tx.ID)
		close(tx.controlReplyCh)
	}()
	msg := tx.newBundleControlMessage(openflow13.OFPBCT_DISCARD_REQUEST)
	if err := tx.sendControlRequest(msg.Header.Xid, msg); err != nil {
		return err
	}
	return nil
}
