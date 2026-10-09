"""moripa-worlds proxy の結合確認用の最小 Minecraft クライアント (標準ライブラリだけ、python3 -I で動く)。

  status HOST PORT VHOST PROTO              サーバーリストの ping を送り、応答 (MOTD) を表示する
  login  HOST PORT VHOST PROTO NAME SECS    オフラインモードでログインし、SECS 秒の間パケットを読む

login は Login Success → Login Acknowledged → CONFIG (Known Packs / Keep Alive / Finish に応答) → PLAY
と進み、PLAY では Keep Alive に応え、Gate がサーバーを移すときの Start Configuration にも応答して
CONFIG → PLAY をもう一度通る。パケット ID は Minecraft 26.3 (protocol 777) のもの
(Gate v0.74.32 の pkg/edition/java/proto/state/register_26_3_test.go と同じ)。
"""
import json
import socket
import struct
import sys
import time
import uuid
import zlib

# 26.3 (777) のパケット ID
LOGIN_DISCONNECT, LOGIN_ENCRYPTION, LOGIN_SUCCESS, LOGIN_COMPRESSION = 0x00, 0x01, 0x02, 0x03
SB_LOGIN_START, SB_LOGIN_ACK = 0x00, 0x03
CFG_DISCONNECT, CFG_FINISH, CFG_KEEPALIVE, CFG_KNOWN_PACKS, CFG_CODE_OF_CONDUCT = 0x02, 0x03, 0x04, 0x0F, 0x14
SB_CFG_FINISH, SB_CFG_KEEPALIVE, SB_CFG_KNOWN_PACKS, SB_CFG_ACCEPT_COC = 0x03, 0x04, 0x07, 0x09
PLAY_DISCONNECT, PLAY_KEEPALIVE, PLAY_JOIN, PLAY_ACTIONBAR, PLAY_START_CONFIG, PLAY_SYSTEM_CHAT = 0x20, 0x2D, 0x32, 0x59, 0x78, 0x7C
SB_PLAY_KEEPALIVE, SB_PLAY_CONFIG_ACK = 0x1C, 0x10


def varint(n):
    out = b""
    n &= 0xFFFFFFFF
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out += bytes([b | 0x80])
        else:
            return out + bytes([b])


def string(s):
    b = s.encode()
    return varint(len(b)) + b


class Buf:
    def __init__(self, b):
        self.b, self.i = b, 0

    def read(self, n):
        r = self.b[self.i:self.i + n]
        if len(r) < n:
            raise EOFError("short buffer")
        self.i += n
        return r

    def varint(self):
        n = sh = 0
        while True:
            b = self.read(1)[0]
            n |= (b & 0x7F) << sh
            sh += 7
            if not b & 0x80:
                return n
            if sh > 35:
                raise ValueError("varint too long")

    def string(self):
        return self.read(self.varint()).decode(errors="replace")

    def rest(self):
        r = self.b[self.i:]
        self.i = len(self.b)
        return r


def nbt_texts(buf):
    """名前なしの NBT (1.20.3+ のチャット部品) から文字列をすべて集める。"""
    out = []

    def payload(t):
        if t == 1: buf.read(1)
        elif t == 2: buf.read(2)
        elif t == 3: buf.read(4)
        elif t == 4: buf.read(8)
        elif t == 5: buf.read(4)
        elif t == 6: buf.read(8)
        elif t == 7: buf.read(struct.unpack(">i", buf.read(4))[0])
        elif t == 8:
            n = struct.unpack(">H", buf.read(2))[0]
            out.append(buf.read(n).decode("utf-8", errors="replace"))
        elif t == 9:
            et = buf.read(1)[0]
            for _ in range(struct.unpack(">i", buf.read(4))[0]):
                payload(et)
        elif t == 10:
            while True:
                ct = buf.read(1)[0]
                if ct == 0:
                    break
                buf.read(struct.unpack(">H", buf.read(2))[0])
                payload(ct)
        elif t == 11: buf.read(4 * struct.unpack(">i", buf.read(4))[0])
        elif t == 12: buf.read(8 * struct.unpack(">i", buf.read(4))[0])
        else:
            raise ValueError("bad nbt tag %d" % t)

    payload(buf.read(1)[0])
    return "".join(out)


class Conn:
    def __init__(self, host, port):
        self.s = socket.create_connection((host, port), timeout=10)
        self.threshold = -1

    def recv_exact(self, n):
        data = b""
        while len(data) < n:
            c = self.s.recv(n - len(data))
            if not c:
                raise EOFError("connection closed")
            data += c
        return data

    def read_varint(self):
        n = sh = 0
        while True:
            b = self.recv_exact(1)[0]
            n |= (b & 0x7F) << sh
            sh += 7
            if not b & 0x80:
                return n

    def read_packet(self):
        data = self.recv_exact(self.read_varint())
        if self.threshold >= 0:
            b = Buf(data)
            dlen = b.varint()
            data = b.rest()
            if dlen:
                data = zlib.decompress(data)
        b = Buf(data)
        return b.varint(), b

    def send(self, pid, payload=b""):
        body = varint(pid) + payload
        if self.threshold >= 0:
            body = varint(0) + body  # 小さいパケットしか送らないので非圧縮
        self.s.sendall(varint(len(body)) + body)


def handshake(c, vhost, proto, next_state):
    c.send(0x00, varint(proto) + string(vhost) + struct.pack(">H", 25565) + varint(next_state))


def do_status(host, port, vhost, proto):
    c = Conn(host, port)
    handshake(c, vhost, proto, 1)
    c.send(0x00)
    pid, b = c.read_packet()
    st = json.loads(b.string())
    desc = st.get("description")
    def flat(d):
        if isinstance(d, str):
            return d
        if isinstance(d, dict):
            return d.get("text", "") + "".join(flat(x) for x in d.get("extra", []))
        return ""
    print("STATUS version=%s protocol=%s players=%s" % (st["version"].get("name"), st["version"].get("protocol"), st.get("players")))
    print("STATUS motd=%r" % flat(desc))


def do_login(host, port, vhost, proto, name, secs):
    c = Conn(host, port)
    handshake(c, vhost, proto, 2)
    c.send(SB_LOGIN_START, string(name) + uuid.uuid3(uuid.NAMESPACE_DNS, name).bytes)
    state = "login"
    joins = 0
    counts = {}
    end = time.time() + secs
    c.s.settimeout(1.0)
    while time.time() < end:
        try:
            pid, b = c.read_packet()
        except socket.timeout:
            continue
        except EOFError:
            print("CLOSED by server (state=%s)" % state)
            break
        key = "%s:0x%02X" % (state, pid)
        counts[key] = counts.get(key, 0) + 1
        if state == "login":
            if pid == LOGIN_COMPRESSION:
                c.threshold = b.varint()
                print("LOGIN set compression threshold=%d" % c.threshold)
            elif pid == LOGIN_DISCONNECT:
                print("LOGIN DISCONNECT %s" % b.string())
                return
            elif pid == LOGIN_ENCRYPTION:
                print("LOGIN encryption request (online mode)")
                return
            elif pid == LOGIN_SUCCESS:
                u = uuid.UUID(bytes=b.read(16))
                print("LOGIN success uuid=%s name=%s" % (u, b.string()))
                c.send(SB_LOGIN_ACK)
                state = "config"
                print("-> CONFIG")
            else:
                print("LOGIN packet 0x%02X" % pid)
        elif state == "config":
            if pid == CFG_KNOWN_PACKS:
                c.send(SB_CFG_KNOWN_PACKS, varint(0))
            elif pid == CFG_KEEPALIVE:
                c.send(SB_CFG_KEEPALIVE, b.read(8))
            elif pid == CFG_CODE_OF_CONDUCT:
                c.send(SB_CFG_ACCEPT_COC)
            elif pid == CFG_DISCONNECT:
                print("CONFIG DISCONNECT %s" % nbt_texts(b))
                return
            elif pid == CFG_FINISH:
                c.send(SB_CFG_FINISH)
                state = "play"
                print("CONFIG finished; config packets so far: %s" % sorted(k for k in counts if k.startswith("config")))
                print("-> PLAY")
        elif state == "play":
            if pid == PLAY_KEEPALIVE:
                c.send(SB_PLAY_KEEPALIVE, b.read(8))
            elif pid == PLAY_JOIN:
                joins += 1
                eid = struct.unpack(">i", b.read(4))[0]
                print("PLAY join game #%d (entity id %d)" % (joins, eid))
            elif pid == PLAY_ACTIONBAR:
                print("PLAY action bar: %s" % nbt_texts(b))
            elif pid == PLAY_SYSTEM_CHAT:
                print("PLAY system chat: %s" % nbt_texts(b))
            elif pid == PLAY_START_CONFIG:
                c.send(SB_PLAY_CONFIG_ACK)
                state = "config"
                print("PLAY start configuration (server switch) -> CONFIG")
            elif pid == PLAY_DISCONNECT:
                print("PLAY DISCONNECT %s" % nbt_texts(b))
                return
    print("SUMMARY joins=%d final_state=%s packets=%s" % (joins, state, json.dumps(counts, sort_keys=True)))


def main(argv):
    if len(argv) >= 5 and argv[1] == "status":
        do_status(argv[2], int(argv[3]), argv[4], int(argv[5]))
    elif len(argv) >= 8 and argv[1] == "login":
        do_login(argv[2], int(argv[3]), argv[4], int(argv[5]), argv[6], float(argv[7]))
    else:
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
