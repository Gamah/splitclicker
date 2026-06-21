using System;
using System.Threading;
using System.Threading.Tasks;
using Sandbox;

namespace Splitclicker.Ws;

// Thin wrapper over Sandbox.WebSocket. Messages arrive on OnMessage; the
// connection drop fires OnDone (the controller reconnects with backoff).
//
// Heartbeat is long (the backend uses protocol-level ping/pong on its own
// cadence); idle costs nothing, which is the whole point of staying connected.
//
// Sends are serialized: the underlying ClientWebSocket allows only one
// outstanding Send at a time and throws if a second starts before the first
// completes. The click path is fire-and-forget by design (no awaiting on the
// hot path), so a fast clicker would otherwise overlap sends and have all but
// the first thrown away. A 1-permit semaphore funnels every frame (clicks +
// ping) through one at a time, in call order, so every click reaches the wire.
public sealed class WsClient : Component
{
	public Action<string> OnMessage { get; set; }
	// Binary frames (the live-window `tick`). The handler receives a COPY of the
	// bytes: the engine hands the callback a Span over a pooled buffer that is
	// reused the instant the callback returns, so we ToArray() before forwarding —
	// the consumer may keep the data past the callback (it's queued into a buffer).
	public Action<byte[]> OnData { get; set; }
	public Action OnDone { get; set; }

	WebSocket _socket;
	readonly SemaphoreSlim _sendGate = new( 1, 1 );
	TimeSince _lastPing;
	const float PingInterval = 60f;

	bool _connected;

	public bool Connected => _connected;

	public async Task Connect( string uri )
	{
		_socket = new WebSocket();
		_socket.OnMessageReceived += msg => OnMessage?.Invoke( msg );
		_socket.OnDataReceived += data => OnData?.Invoke( data.ToArray() );
		_socket.OnDisconnected += ( status, reason ) =>
		{
			_connected = false;
			OnDone?.Invoke();
		};

		await _socket.Connect( uri );
		_connected = true;
		_lastPing = 0;
	}

	// Send serializes through _sendGate so overlapping fire-and-forget calls
	// (rapid clicks) are delivered one after another instead of throwing on a
	// concurrent ClientWebSocket.Send. Order is preserved: the gate hands out
	// its single permit in the order WaitAsync was called.
	public async Task Send( string message )
	{
		if ( _socket == null || !_connected ) return;
		await _sendGate.WaitAsync();
		try
		{
			if ( _socket != null && _connected )
				await _socket.Send( message );
		}
		catch ( Exception e )
		{
			Log.Warning( $"[Splitclicker] ws send failed: {e.Message}" );
		}
		finally
		{
			_sendGate.Release();
		}
	}

	public void Disconnect()
	{
		_socket?.Dispose();
		_socket = null;
		_connected = false;
	}

	protected override void OnUpdate()
	{
		if ( !_connected || _socket == null ) return;
		if ( _lastPing > PingInterval )
		{
			_ = Send( "{\"t\":\"ping\"}" );
			_lastPing = 0;
		}
	}

	protected override void OnDestroy() => Disconnect();
}
