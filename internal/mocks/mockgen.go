package mocks

//go:generate sh -c "mockgen -package mockquic -destination quic/stream.go github.com/DrakenLibra/gt-bbr Stream && goimports -w quic/stream.go"
//go:generate sh -c "mockgen -package mockquic -destination quic/session.go github.com/DrakenLibra/gt-bbr Session && goimports -w quic/session.go"
//go:generate sh -c "../mockgen_internal.sh mocks sealer.go github.com/DrakenLibra/gt-bbr/internal/handshake Sealer"
//go:generate sh -c "../mockgen_internal.sh mocks opener.go github.com/DrakenLibra/gt-bbr/internal/handshake Opener"
//go:generate sh -c "../mockgen_internal.sh mocks crypto_setup.go github.com/DrakenLibra/gt-bbr/internal/handshake CryptoSetup"
//go:generate sh -c "../mockgen_internal.sh mocks stream_flow_controller.go github.com/DrakenLibra/gt-bbr/internal/flowcontrol StreamFlowController"
//go:generate sh -c "../mockgen_internal.sh mockackhandler ackhandler/sent_packet_handler.go github.com/DrakenLibra/gt-bbr/internal/ackhandler SentPacketHandler"
//go:generate sh -c "../mockgen_internal.sh mockackhandler ackhandler/received_packet_handler.go github.com/DrakenLibra/gt-bbr/internal/ackhandler ReceivedPacketHandler"
//go:generate sh -c "../mockgen_internal.sh mocks congestion.go github.com/DrakenLibra/gt-bbr/internal/congestion SendAlgorithmWithDebugInfos"
//go:generate sh -c "../mockgen_internal.sh mocks connection_flow_controller.go github.com/DrakenLibra/gt-bbr/internal/flowcontrol ConnectionFlowController"
