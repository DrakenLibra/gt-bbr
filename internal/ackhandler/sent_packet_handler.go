package ackhandler

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/DrakenLibra/gt-bbr/internal/congestion"
	"github.com/DrakenLibra/gt-bbr/internal/protocol"
	"github.com/DrakenLibra/gt-bbr/internal/qerr"
	"github.com/DrakenLibra/gt-bbr/internal/utils"
	"github.com/DrakenLibra/gt-bbr/internal/wire"
)

const (
	// Maximum reordering in time space before time based loss detection considers a packet lost.
	// Specified as an RTT multiplier.
	timeThreshold = 9.0 / 8
)

type packetNumberSpace struct {
	history *sentPacketHistory
	pns     *packetNumberGenerator

	largestAcked protocol.PacketNumber
	largestSent  protocol.PacketNumber
}

func newPacketNumberSpace(initialPN protocol.PacketNumber) *packetNumberSpace {
	return &packetNumberSpace{
		history:      newSentPacketHistory(),
		pns:          newPacketNumberGenerator(initialPN, protocol.SkipPacketAveragePeriodLength),
		largestSent:  protocol.InvalidPacketNumber,
		largestAcked: protocol.InvalidPacketNumber,
	}
}

type sentPacketHandler struct {
	lastSentAckElicitingPacketTime time.Time // only applies to the application-data packet number space
	lastSentCryptoPacketTime       time.Time

	nextSendTime time.Time

	initialPackets   *packetNumberSpace
	handshakePackets *packetNumberSpace
	oneRTTPackets    *packetNumberSpace

	// lowestNotConfirmedAcked is the lowest packet number that we sent an ACK for, but haven't received confirmation, that this ACK actually arrived
	// example: we send an ACK for packets 90-100 with packet number 20
	// once we receive an ACK from the peer for packet 20, the lowestNotConfirmedAcked is 101
	// Only applies to the application-data packet number space.
	lowestNotConfirmedAcked protocol.PacketNumber

	retransmissionQueue []*Packet

	bytesInFlight protocol.ByteCount

	congestion congestion.SendAlgorithmWithDebugInfos
	rttStats   *congestion.RTTStats

	maxAckDelay time.Duration

	// The number of times the crypto packets have been retransmitted without receiving an ack.
	cryptoCount uint32
	// The number of times a PTO has been sent without receiving an ack.
	ptoCount uint32
	// The number of PTO probe packets that should be sent.
	// Only applies to the application-data packet number space.
	numProbesToSend int

	// The time at which the next packet will be considered lost based on early transmit or exceeding the reordering window in time.
	lossTime time.Time

	// The alarm timeout
	alarm time.Time

	logger utils.Logger
}

// NewSentPacketHandler creates a new sentPacketHandler
func NewSentPacketHandler(
	initialPacketNumber protocol.PacketNumber,
	rttStats *congestion.RTTStats,
	logger utils.Logger,
) SentPacketHandler {
	handler := &sentPacketHandler{
		initialPackets:   newPacketNumberSpace(initialPacketNumber),
		handshakePackets: newPacketNumberSpace(0),
		oneRTTPackets:    newPacketNumberSpace(0),
		rttStats:         rttStats,
		logger:           logger,
	}
	cc := congestion.NewBBRSender(congestion.DefaultClock{},
		rttStats,
		protocol.InitialCongestionWindow,
		protocol.DefaultBBRMaxCongestionWindow,
		func() protocol.ByteCount {
			return handler.bytesInFlight
		},
	)
	handler.congestion = cc
	return handler

}

func (h *sentPacketHandler) DropPackets(encLevel protocol.EncryptionLevel) {
	// remove outstanding packets from bytes_in_flight
	pnSpace := h.getPacketNumberSpace(encLevel)
	pnSpace.history.Iterate(func(p *Packet) (bool, error) {
		if p.includedInBytesInFlight {
			h.bytesInFlight -= p.Length
		}
		return true, nil
	})
	// remove packets from the retransmission queue
	var queue []*Packet
	for _, packet := range h.retransmissionQueue {
		if packet.EncryptionLevel != encLevel {
			queue = append(queue, packet)
		}
	}
	h.retransmissionQueue = queue
	// drop the packet history
	switch encLevel {
	case protocol.EncryptionInitial:
		h.initialPackets = nil
	case protocol.EncryptionHandshake:
		h.handshakePackets = nil
	default:
		panic(fmt.Sprintf("Cannot drop keys for encryption level %s", encLevel))
	}
}

func (h *sentPacketHandler) SetMaxAckDelay(mad time.Duration) {
	h.maxAckDelay = mad
}

func (h *sentPacketHandler) SentPacket(packet *Packet) {
	if isAckEliciting := h.sentPacketImpl(packet); isAckEliciting {
		h.getPacketNumberSpace(packet.EncryptionLevel).history.SentPacket(packet)
		h.updateLossDetectionAlarm()
	}
}

func (h *sentPacketHandler) SentPacketsAsRetransmission(packets []*Packet, retransmissionOf protocol.PacketNumber) {
	var p []*Packet
	for _, packet := range packets {
		if isAckEliciting := h.sentPacketImpl(packet); isAckEliciting {
			p = append(p, packet)
		}
	}
	h.getPacketNumberSpace(p[0].EncryptionLevel).history.SentPacketsAsRetransmission(p, retransmissionOf)
	h.updateLossDetectionAlarm()
}

func (h *sentPacketHandler) getPacketNumberSpace(encLevel protocol.EncryptionLevel) *packetNumberSpace {
	switch encLevel {
	case protocol.EncryptionInitial:
		return h.initialPackets
	case protocol.EncryptionHandshake:
		return h.handshakePackets
	case protocol.Encryption1RTT:
		return h.oneRTTPackets
	default:
		panic("invalid packet number space")
	}
}

func (h *sentPacketHandler) sentPacketImpl(packet *Packet) bool /* is ack-eliciting */ {
	pnSpace := h.getPacketNumberSpace(packet.EncryptionLevel)

	if h.logger.Debug() {
		for p := utils.MaxPacketNumber(0, pnSpace.largestSent+1); p < packet.PacketNumber; p++ {
			h.logger.Debugf("Skipping packet number %#x", p)
		}
	}

	pnSpace.largestSent = packet.PacketNumber

	packet.largestAcked = protocol.InvalidPacketNumber
	if packet.Ack != nil {
		packet.largestAcked = packet.Ack.LargestAcked()
	}
	packet.Ack = nil // no need to save the ACK

	isAckEliciting := len(packet.Frames) > 0

	if isAckEliciting {
		if packet.EncryptionLevel != protocol.Encryption1RTT {
			h.lastSentCryptoPacketTime = packet.SendTime
		}
		h.lastSentAckElicitingPacketTime = packet.SendTime
		packet.includedInBytesInFlight = true
		h.bytesInFlight += packet.Length
		packet.canBeRetransmitted = true
		if h.numProbesToSend > 0 {
			h.numProbesToSend--
		}
	}
	h.congestion.OnPacketSent(packet.SendTime, h.bytesInFlight, packet.PacketNumber, packet.Length, isAckEliciting)

	h.nextSendTime = utils.MaxTime(h.nextSendTime, packet.SendTime).Add(h.congestion.TimeUntilSend(h.bytesInFlight))
	return isAckEliciting
}

func (h *sentPacketHandler) ReceivedAck(ackFrame *wire.AckFrame, withPacketNumber protocol.PacketNumber, encLevel protocol.EncryptionLevel, rcvTime time.Time) error {
	pnSpace := h.getPacketNumberSpace(encLevel)

	largestAcked := ackFrame.LargestAcked()
	if largestAcked > pnSpace.largestSent {
		return qerr.Error(qerr.ProtocolViolation, "Received ACK for an unsent packet")
	}

	pnSpace.largestAcked = utils.MaxPacketNumber(pnSpace.largestAcked, largestAcked)

	if !pnSpace.pns.Validate(ackFrame) {
		return qerr.Error(qerr.ProtocolViolation, "Received an ACK for a skipped packet number")
	}

	// maybe update the RTT
	if p := pnSpace.history.GetPacket(ackFrame.LargestAcked()); p != nil {
		// don't use the ack delay for Initial and Handshake packets
		var ackDelay time.Duration
		if encLevel == protocol.Encryption1RTT {
			ackDelay = utils.MinDuration(ackFrame.DelayTime, h.maxAckDelay)
		}
		h.rttStats.UpdateRTT(rcvTime.Sub(p.SendTime), ackDelay, rcvTime)
		if h.logger.Debug() {
			h.logger.Debugf("\tupdated RTT: %s (σ: %s)", h.rttStats.SmoothedRTT(), h.rttStats.MeanDeviation())
		}
		h.congestion.MaybeExitSlowStart()
	}

	ackedPackets, err := h.determineNewlyAckedPackets(ackFrame, encLevel)
	if err != nil {
		return err
	}
	if len(ackedPackets) == 0 {
		return nil
	}

	// has impled CongestionEvent interface.
	congestionEventHandler, hasCongestionEvent := h.congestion.(congestion.CongestionEvent)
	var ackedPacketsForEvent, lostPacketsForEvent []*protocol.Packet

	priorInFlight := h.bytesInFlight
	for _, p := range ackedPackets {
		if p.largestAcked != protocol.InvalidPacketNumber && encLevel == protocol.Encryption1RTT {
			h.lowestNotConfirmedAcked = utils.MaxPacketNumber(h.lowestNotConfirmedAcked, p.largestAcked+1)
		}
		if err := h.onPacketAcked(p, rcvTime); err != nil {
			return err
		}
		if p.includedInBytesInFlight {
			if hasCongestionEvent {
				ackedPacketsForEvent = append(ackedPacketsForEvent, p.ToPacket())
			} else {
				h.congestion.OnPacketAcked(p.PacketNumber, p.Length, priorInFlight, rcvTime)
			}
		}
	}

	lostPackets, err := h.detectLostPackets(rcvTime, encLevel, priorInFlight)
	if hasCongestionEvent {
		if lostPackets != nil {
			lostPacketsForEvent := make([]*protocol.Packet, len(lostPackets))
			for idx, p := range lostPackets {
				lostPacketsForEvent[idx] = p.ToPacket()
			}
		}
		congestionEventHandler.OnCongestionEvent(priorInFlight, rcvTime, ackedPacketsForEvent, lostPacketsForEvent)
	}

	if err != nil {
		return err
	}

	h.ptoCount = 0
	h.cryptoCount = 0
	h.numProbesToSend = 0

	h.updateLossDetectionAlarm()
	return nil
}

func (h *sentPacketHandler) GetLowestPacketNotConfirmedAcked() protocol.PacketNumber {
	return h.lowestNotConfirmedAcked
}

func (h *sentPacketHandler) determineNewlyAckedPackets(
	ackFrame *wire.AckFrame,
	encLevel protocol.EncryptionLevel,
) ([]*Packet, error) {
	pnSpace := h.getPacketNumberSpace(encLevel)
	var ackedPackets []*Packet
	ackRangeIndex := 0
	lowestAcked := ackFrame.LowestAcked()
	largestAcked := ackFrame.LargestAcked()
	err := pnSpace.history.Iterate(func(p *Packet) (bool, error) {
		// Ignore packets below the lowest acked
		if p.PacketNumber < lowestAcked {
			return true, nil
		}
		// Break after largest acked is reached
		if p.PacketNumber > largestAcked {
			return false, nil
		}

		if ackFrame.HasMissingRanges() {
			ackRange := ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]

			for p.PacketNumber > ackRange.Largest && ackRangeIndex < len(ackFrame.AckRanges)-1 {
				ackRangeIndex++
				ackRange = ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]
			}

			if p.PacketNumber >= ackRange.Smallest { // packet i contained in ACK range
				if p.PacketNumber > ackRange.Largest {
					return false, fmt.Errorf("BUG: ackhandler would have acked wrong packet 0x%x, while evaluating range 0x%x -> 0x%x", p.PacketNumber, ackRange.Smallest, ackRange.Largest)
				}
				ackedPackets = append(ackedPackets, p)
			}
		} else {
			ackedPackets = append(ackedPackets, p)
		}
		return true, nil
	})
	if h.logger.Debug() && len(ackedPackets) > 0 {
		pns := make([]protocol.PacketNumber, len(ackedPackets))
		for i, p := range ackedPackets {
			pns[i] = p.PacketNumber
		}
		h.logger.Debugf("\tnewly acked packets (%d): %#x", len(pns), pns)
	}
	return ackedPackets, err
}

func (h *sentPacketHandler) hasOutstandingCryptoPackets() bool {
	var hasInitial, hasHandshake bool
	if h.initialPackets != nil {
		hasInitial = h.initialPackets.history.HasOutstandingPackets()
	}
	if h.handshakePackets != nil {
		hasHandshake = h.handshakePackets.history.HasOutstandingPackets()
	}
	return hasInitial || hasHandshake
}

func (h *sentPacketHandler) hasOutstandingPackets() bool {
	return h.oneRTTPackets.history.HasOutstandingPackets() || h.hasOutstandingCryptoPackets()
}

func (h *sentPacketHandler) updateLossDetectionAlarm() {
	// Cancel the alarm if no packets are outstanding
	if !h.hasOutstandingPackets() {
		h.alarm = time.Time{}
		return
	}

	if h.hasOutstandingCryptoPackets() {
		h.alarm = h.lastSentCryptoPacketTime.Add(h.computeCryptoTimeout())
	} else if !h.lossTime.IsZero() {
		// Early retransmit timer or time loss detection.
		h.alarm = h.lossTime
	} else { // PTO alarm
		h.alarm = h.lastSentAckElicitingPacketTime.Add(h.computePTOTimeout())
	}
}

func (h *sentPacketHandler) detectLostPackets(
	now time.Time,
	encLevel protocol.EncryptionLevel,
	priorInFlight protocol.ByteCount,
) ([]*Packet, error) {
	if encLevel == protocol.Encryption1RTT {
		h.lossTime = time.Time{}
	}
	pnSpace := h.getPacketNumberSpace(encLevel)

	maxRTT := float64(utils.MaxDuration(h.rttStats.LatestRTT(), h.rttStats.SmoothedRTT()))
	lossDelay := time.Duration(timeThreshold * maxRTT)

	// Minimum time of granularity before packets are deemed lost.
	lossDelay = utils.MaxDuration(lossDelay, protocol.TimerGranularity)

	var lostPackets []*Packet
	pnSpace.history.Iterate(func(packet *Packet) (bool, error) {
		if packet.PacketNumber > pnSpace.largestAcked {
			return false, nil
		}

		timeSinceSent := now.Sub(packet.SendTime)
		if timeSinceSent > lossDelay {
			lostPackets = append(lostPackets, packet)
		} else if h.lossTime.IsZero() && encLevel == protocol.Encryption1RTT {
			if h.logger.Debug() {
				h.logger.Debugf("\tsetting loss timer for packet %#x to %s (in %s)", packet.PacketNumber, lossDelay, lossDelay-timeSinceSent)
			}
			// Note: This conditional is only entered once per call
			h.lossTime = now.Add(lossDelay - timeSinceSent)
		}
		return true, nil
	})

	if h.logger.Debug() && len(lostPackets) > 0 {
		pns := make([]protocol.PacketNumber, len(lostPackets))
		for i, p := range lostPackets {
			pns[i] = p.PacketNumber
		}
		h.logger.Debugf("\tlost packets (%d): %#x", len(pns), pns)
	}

	// has impled CongestionEvent interface.
	_, hasCongestionEvent := h.congestion.(congestion.CongestionEvent)

	for _, p := range lostPackets {
		// the bytes in flight need to be reduced no matter if this packet will be retransmitted
		if p.includedInBytesInFlight {
			h.bytesInFlight -= p.Length
			if !hasCongestionEvent {
				h.congestion.OnPacketLost(p.PacketNumber, p.Length, priorInFlight)
			}
		}
		if p.canBeRetransmitted {
			// queue the packet for retransmission, and report the loss to the congestion controller
			if err := h.queuePacketForRetransmission(p, pnSpace); err != nil {
				return nil, err
			}
		}
		pnSpace.history.Remove(p.PacketNumber)
	}
	return lostPackets, nil
}

func (h *sentPacketHandler) OnAlarm() error {
	// When all outstanding are acknowledged, the alarm is canceled in
	// updateLossDetectionAlarm. This doesn't reset the timer in the session though.
	// When OnAlarm is called, we therefore need to make sure that there are
	// actually packets outstanding.
	if h.hasOutstandingPackets() {
		if err := h.onVerifiedAlarm(); err != nil {
			return err
		}
	}
	h.updateLossDetectionAlarm()
	return nil
}

func (h *sentPacketHandler) onVerifiedAlarm() error {
	var err error
	if h.hasOutstandingCryptoPackets() {
		if h.logger.Debug() {
			h.logger.Debugf("Loss detection alarm fired in crypto mode. Crypto count: %d", h.cryptoCount)
		}
		h.cryptoCount++
		err = h.queueCryptoPacketsForRetransmission()
	} else if !h.lossTime.IsZero() {
		if h.logger.Debug() {
			h.logger.Debugf("Loss detection alarm fired in loss timer mode. Loss time: %s", h.lossTime)
		}

		// Early retransmit or time loss detection
		var lostPackets []*Packet
		priorInFlight := h.bytesInFlight

		lostPackets, err = h.detectLostPackets(time.Now(), protocol.Encryption1RTT, priorInFlight)
		if congestionEventHandler, ok := h.congestion.(congestion.CongestionEvent); ok && lostPackets != nil {
			lostPacketsForEvent := make([]*protocol.Packet, len(lostPackets))
			for idx, p := range lostPackets {
				lostPacketsForEvent[idx] = p.ToPacket()
			}
			congestionEventHandler.OnCongestionEvent(priorInFlight, time.Now(), nil, lostPacketsForEvent)
		}
	} else { // PTO
		if h.logger.Debug() {
			h.logger.Debugf("Loss detection alarm fired in PTO mode. PTO count: %d", h.ptoCount)
		}
		h.ptoCount++
		h.numProbesToSend += 2
	}
	return err
}

func (h *sentPacketHandler) GetAlarmTimeout() time.Time {
	return h.alarm
}

func (h *sentPacketHandler) onPacketAcked(p *Packet, rcvTime time.Time) error {
	pnSpace := h.getPacketNumberSpace(p.EncryptionLevel)
	// This happens if a packet and its retransmissions is acked in the same ACK.
	// As soon as we process the first one, this will remove all the retransmissions,
	// so we won't find the retransmitted packet number later.
	if packet := pnSpace.history.GetPacket(p.PacketNumber); packet == nil {
		return nil
	}

	// only report the acking of this packet to the congestion controller if:
	// * it is an ack-eliciting packet
	// * this packet wasn't retransmitted yet
	if p.isRetransmission {
		// that the parent doesn't exist is expected to happen every time the original packet was already acked
		if parent := pnSpace.history.GetPacket(p.retransmissionOf); parent != nil {
			if len(parent.retransmittedAs) == 1 {
				parent.retransmittedAs = nil
			} else {
				// remove this packet from the slice of retransmission
				retransmittedAs := make([]protocol.PacketNumber, 0, len(parent.retransmittedAs)-1)
				for _, pn := range parent.retransmittedAs {
					if pn != p.PacketNumber {
						retransmittedAs = append(retransmittedAs, pn)
					}
				}
				parent.retransmittedAs = retransmittedAs
			}
		}
	}
	// this also applies to packets that have been retransmitted as probe packets
	if p.includedInBytesInFlight {
		h.bytesInFlight -= p.Length
	}
	if err := h.stopRetransmissionsFor(p, pnSpace); err != nil {
		return err
	}
	return pnSpace.history.Remove(p.PacketNumber)
}

func (h *sentPacketHandler) stopRetransmissionsFor(p *Packet, pnSpace *packetNumberSpace) error {
	if err := pnSpace.history.MarkCannotBeRetransmitted(p.PacketNumber); err != nil {
		return err
	}
	for _, r := range p.retransmittedAs {
		packet := pnSpace.history.GetPacket(r)
		if packet == nil {
			return fmt.Errorf("sent packet handler BUG: marking packet as not retransmittable %d (retransmission of %d) not found in history", r, p.PacketNumber)
		}
		h.stopRetransmissionsFor(packet, pnSpace)
	}
	return nil
}

func (h *sentPacketHandler) DequeuePacketForRetransmission() *Packet {
	if len(h.retransmissionQueue) == 0 {
		return nil
	}
	packet := h.retransmissionQueue[0]
	// Shift the slice and don't retain anything that isn't needed.
	copy(h.retransmissionQueue, h.retransmissionQueue[1:])
	h.retransmissionQueue[len(h.retransmissionQueue)-1] = nil
	h.retransmissionQueue = h.retransmissionQueue[:len(h.retransmissionQueue)-1]
	return packet
}

func (h *sentPacketHandler) DequeueProbePacket() (*Packet, error) {
	pnSpace := h.getPacketNumberSpace(protocol.Encryption1RTT)
	if len(h.retransmissionQueue) == 0 {
		p := pnSpace.history.FirstOutstanding()
		if p == nil {
			return nil, errors.New("cannot dequeue a probe packet. No outstanding packets")
		}
		if err := h.queuePacketForRetransmission(p, pnSpace); err != nil {
			return nil, err
		}
	}
	return h.DequeuePacketForRetransmission(), nil
}

func (h *sentPacketHandler) PeekPacketNumber(encLevel protocol.EncryptionLevel) (protocol.PacketNumber, protocol.PacketNumberLen) {
	pnSpace := h.getPacketNumberSpace(encLevel)

	var lowestUnacked protocol.PacketNumber
	if p := pnSpace.history.FirstOutstanding(); p != nil {
		lowestUnacked = p.PacketNumber
	} else {
		lowestUnacked = pnSpace.largestAcked + 1
	}

	pn := pnSpace.pns.Peek()
	return pn, protocol.GetPacketNumberLengthForHeader(pn, lowestUnacked)
}

func (h *sentPacketHandler) PopPacketNumber(encLevel protocol.EncryptionLevel) protocol.PacketNumber {
	return h.getPacketNumberSpace(encLevel).pns.Pop()
}

func (h *sentPacketHandler) SendMode() SendMode {
	numTrackedPackets := len(h.retransmissionQueue) + h.oneRTTPackets.history.Len()
	if h.initialPackets != nil {
		numTrackedPackets += h.initialPackets.history.Len()
	}
	if h.handshakePackets != nil {
		numTrackedPackets += h.handshakePackets.history.Len()
	}

	// Don't send any packets if we're keeping track of the maximum number of packets.
	// Note that since MaxOutstandingSentPackets is smaller than MaxTrackedSentPackets,
	// we will stop sending out new data when reaching MaxOutstandingSentPackets,
	// but still allow sending of retransmissions and ACKs.
	if numTrackedPackets >= protocol.MaxTrackedSentPackets {
		if h.logger.Debug() {
			h.logger.Debugf("Limited by the number of tracked packets: tracking %d packets, maximum %d", numTrackedPackets, protocol.MaxTrackedSentPackets)
		}
		return SendNone
	}
	if h.numProbesToSend > 0 {
		return SendPTO
	}
	// Only send ACKs if we're congestion limited.
	if !h.congestion.CanSend(h.bytesInFlight) {
		if h.logger.Debug() {
			h.logger.Debugf("Congestion limited: bytes in flight %d, window %d", h.bytesInFlight, h.congestion.GetCongestionWindow())
		}
		return SendAck
	}
	// Send retransmissions first, if there are any.
	if len(h.retransmissionQueue) > 0 {
		return SendRetransmission
	}
	if numTrackedPackets >= protocol.MaxOutstandingSentPackets {
		if h.logger.Debug() {
			h.logger.Debugf("Max outstanding limited: tracking %d packets, maximum: %d", numTrackedPackets, protocol.MaxOutstandingSentPackets)
		}
		return SendAck
	}
	return SendAny
}

func (h *sentPacketHandler) TimeUntilSend() time.Time {
	return h.nextSendTime
}

func (h *sentPacketHandler) ShouldSendNumPackets() int {
	if h.numProbesToSend > 0 {
		// RTO probes should not be paced, but must be sent immediately.
		return h.numProbesToSend
	}
	delay := h.congestion.TimeUntilSend(h.bytesInFlight)
	if delay == 0 || delay > protocol.MinPacingDelay {
		return 1
	}
	return int(math.Ceil(float64(protocol.MinPacingDelay) / float64(delay)))
}

func (h *sentPacketHandler) queueCryptoPacketsForRetransmission() error {
	if err := h.queueAllPacketsForRetransmission(protocol.EncryptionInitial); err != nil {
		return err
	}
	return h.queueAllPacketsForRetransmission(protocol.EncryptionHandshake)
}

func (h *sentPacketHandler) queueAllPacketsForRetransmission(encLevel protocol.EncryptionLevel) error {
	var packets []*Packet
	pnSpace := h.getPacketNumberSpace(encLevel)
	pnSpace.history.Iterate(func(p *Packet) (bool, error) {
		if p.canBeRetransmitted {
			packets = append(packets, p)
		}
		return true, nil
	})
	for _, p := range packets {
		h.logger.Debugf("Queueing packet %#x (%s) as a crypto retransmission", p.PacketNumber, encLevel)
		if err := h.queuePacketForRetransmission(p, pnSpace); err != nil {
			return err
		}
	}
	return nil
}

func (h *sentPacketHandler) queuePacketForRetransmission(p *Packet, pnSpace *packetNumberSpace) error {
	if !p.canBeRetransmitted {
		return fmt.Errorf("sent packet handler BUG: packet %d already queued for retransmission", p.PacketNumber)
	}
	if err := pnSpace.history.MarkCannotBeRetransmitted(p.PacketNumber); err != nil {
		return err
	}
	h.retransmissionQueue = append(h.retransmissionQueue, p)
	return nil
}

func (h *sentPacketHandler) computeCryptoTimeout() time.Duration {
	duration := utils.MaxDuration(2*h.rttStats.SmoothedOrInitialRTT(), protocol.TimerGranularity)
	// exponential backoff
	// There's an implicit limit to this set by the crypto timeout.
	return duration << h.cryptoCount
}

func (h *sentPacketHandler) computePTOTimeout() time.Duration {
	duration := h.rttStats.SmoothedOrInitialRTT() + utils.MaxDuration(4*h.rttStats.MeanDeviation(), protocol.TimerGranularity) + h.maxAckDelay
	return duration << h.ptoCount
}

func (h *sentPacketHandler) ResetForRetry() error {
	h.cryptoCount = 0
	h.bytesInFlight = 0
	var packets []*Packet
	h.initialPackets.history.Iterate(func(p *Packet) (bool, error) {
		if p.canBeRetransmitted {
			packets = append(packets, p)
		}
		return true, nil
	})
	for _, p := range packets {
		h.logger.Debugf("Queueing packet %#x for retransmission.", p.PacketNumber)
		h.retransmissionQueue = append(h.retransmissionQueue, p)
	}
	h.initialPackets = newPacketNumberSpace(h.initialPackets.pns.Pop())
	h.updateLossDetectionAlarm()
	return nil
}
