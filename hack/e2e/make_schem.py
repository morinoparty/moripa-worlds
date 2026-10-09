"""Write a tiny Sponge v3 schematic (5x2x5 stone floor + glass) as gzip NBT."""
import gzip, struct, sys

def s(name):  # NBT string payload
    b = name.encode(); return struct.pack(">H", len(b)) + b
def tag(t, name, payload): return bytes([t]) + s(name) + payload
def short(n, v): return tag(2, n, struct.pack(">h", v))
def int_(n, v): return tag(3, n, struct.pack(">i", v))
def bytearr(n, data): return tag(7, n, struct.pack(">i", len(data)) + bytes(data))
def intarr(n, vals): return tag(11, n, struct.pack(">i", len(vals)) + b"".join(struct.pack(">i", v) for v in vals))
def compound(n, *children): return tag(10, n, b"".join(children) + b"\x00")
def list_empty(n, elem=10): return tag(9, n, bytes([elem]) + struct.pack(">i", 0))

W, H, L = 5, 2, 5
palette = {"minecraft:stone": 0, "minecraft:glass": 1}
data = []
for y in range(H):
    for z in range(L):
        for x in range(W):
            data.append(0 if y == 0 else (1 if (x in (0, W-1) or z in (0, L-1)) else 2))
palette["minecraft:air"] = 2
root = compound("", compound("Schematic",
    int_("Version", 3), int_("DataVersion", 4325),
    short("Width", W), short("Height", H), short("Length", L),
    intarr("Offset", [0, 0, 0]),
    compound("Blocks",
        compound("Palette", *[int_(k, v) for k, v in palette.items()]),
        bytearr("Data", data),  # all ids < 128, so one varint byte each
        list_empty("BlockEntities"))))
with gzip.open(sys.argv[1], "wb") as f:
    f.write(root)
