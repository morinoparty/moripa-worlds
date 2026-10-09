"""Gate 経由で waker に status ping / login start を送る確認用クライアント (python3 -I で実行)。

usage: python3 -I mcclient.py HOST PORT SERVER_ADDRESS status|login [--proxy-v2]
"""
import json
import socket
import struct
import sys


def varint(v):
    v &= 0xFFFFFFFF
    out = bytearray()
    while True:
        b = v & 0x7F
        v >>= 7
        if v:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def read_varint(sock):
    result = 0
    for i in range(5):
        b = sock.recv(1)
        if not b:
            raise EOFError("connection closed")
        result |= (b[0] & 0x7F) << (7 * i)
        if not b[0] & 0x80:
            return result
    raise ValueError("varint too big")


def packet(pid, data=b""):
    body = varint(pid) + data
    return varint(len(body)) + body


def mcstring(s):
    b = s.encode("utf-8")
    return varint(len(b)) + b


def read_packet(sock):
    n = read_varint(sock)
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise EOFError("short packet")
        buf += chunk
    # packet id は 1 バイト (0x00 / 0x01) 前提
    return buf[0], buf[1:]


def read_string(data):
    n, i = 0, 0
    for i in range(5):
        n |= (data[i] & 0x7F) << (7 * i)
        if not data[i] & 0x80:
            break
    return data[i + 1 : i + 1 + n].decode("utf-8")


def proxy_v2(src=("203.0.113.7", 54321), dst=("127.0.0.1", 35565)):
    sig = b"\r\n\r\n\x00\r\nQUIT\n"
    addrs = socket.inet_aton(src[0]) + socket.inet_aton(dst[0]) + struct.pack(">HH", src[1], dst[1])
    return sig + bytes([0x21, 0x11]) + struct.pack(">H", len(addrs)) + addrs


def main():
    host, port, server_address, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
    server_address = server_address.replace("\\0", "\x00")
    use_proxy = "--proxy-v2" in sys.argv
    protocol = 773
    sock = socket.create_connection((host, port), timeout=10)
    if use_proxy:
        sock.sendall(proxy_v2())
    next_state = 1 if mode == "status" else 2
    hs = varint(protocol) + mcstring(server_address) + struct.pack(">H", port) + varint(next_state)
    sock.sendall(packet(0x00, hs))
    if mode == "status":
        sock.sendall(packet(0x00))
        pid, data = read_packet(sock)
        status = json.loads(read_string(data))
        print("STATUS", json.dumps(status, ensure_ascii=False))
        sock.sendall(packet(0x01, struct.pack(">q", 1234567)))
        pid, data = read_packet(sock)
        print("PONG", pid, struct.unpack(">q", data)[0])
    else:
        sock.sendall(packet(0x00, mcstring("Steve") + bytes(16)))
        pid, data = read_packet(sock)
        print("DISCONNECT", hex(pid), json.dumps(json.loads(read_string(data)), ensure_ascii=False))
    sock.close()


if __name__ == "__main__":
    main()
