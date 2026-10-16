#!/usr/bin/env python3
"""Generates the demo files uploaded by scripts/screenshots/seed.sh.

Deterministic (fixed seed) so that the screenshots are reproducible. Only the
standard library is required; when Pillow is installed the photos are real
JPEG images, otherwise they are PNG gradients saved with the same names.
"""

import gzip
import json
import math
import os
import random
import struct
import sys
import zlib

rng = random.Random(20261016)


def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    mode = "wb" if isinstance(data, bytes) else "w"
    with open(path, mode) as f:
        f.write(data)


def png_bytes(width, height, c1, c2):
    rows = []
    for y in range(height):
        row = bytearray([0])
        for x in range(width):
            t = (x / width + y / height) / 2
            w = 0.5 + 0.5 * math.sin((x + y) / 23.0)
            row += bytes(
                int(c1[i] * (1 - t) + c2[i] * t * (0.85 + 0.15 * w)) for i in range(3)
            )
        rows.append(bytes(row))
    raw = b"".join(rows)

    def chunk(kind, body):
        return (
            struct.pack(">I", len(body))
            + kind
            + body
            + struct.pack(">I", zlib.crc32(kind + body) & 0xFFFFFFFF)
        )

    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw, 6))
        + chunk(b"IEND", b"")
    )


PALETTES = [
    ((255, 153, 102), (102, 51, 153)),  # sunset
    ((0, 119, 182), (144, 224, 239)),  # sea
    ((45, 106, 79), (183, 228, 199)),  # forest
    ((230, 57, 70), (241, 250, 238)),  # coral
    ((38, 70, 83), (233, 196, 106)),  # dune
    ((20, 33, 61), (252, 163, 17)),  # night
]


def photo(path, width, height, palette, quality=88):
    c1, c2 = palette
    try:
        from PIL import Image, ImageDraw, ImageFilter  # type: ignore

        img = Image.new("RGB", (width, height))
        draw = ImageDraw.Draw(img)
        for y in range(height):
            t = y / height
            draw.line(
                [(0, y), (width, y)],
                fill=tuple(int(c1[i] * (1 - t) + c2[i] * t) for i in range(3)),
            )
        for _ in range(40):
            r = rng.randint(width // 40, width // 8)
            x, y = rng.randint(0, width), rng.randint(0, height)
            shade = tuple(min(255, int(v * rng.uniform(0.7, 1.3))) for v in c2)
            draw.ellipse([x - r, y - r, x + r, y + r], fill=shade)
        img = img.filter(ImageFilter.GaussianBlur(radius=width // 160))
        # Grain, so that the JPEG sizes look like real photos.
        noise = Image.effect_noise((width, height), 28).convert("RGB")
        img = Image.blend(img, noise, 0.08)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        img.save(path, "JPEG", quality=quality)
    except ImportError:
        write(path, png_bytes(min(width, 480), min(height, 320), c1, c2))


def random_bytes(n):
    return rng.randbytes(n)


def log_lines(service, day, count):
    levels = ["INFO"] * 12 + ["WARN"] * 2 + ["ERROR"]
    paths = ["/", "/about/", "/assets/css/site.css", "/assets/js/app.js", "/api/status"]
    out = []
    for i in range(count):
        out.append(
            f"2026-10-{day:02d}T{(i // 120) % 24:02d}:{(i // 2) % 60:02d}:{i % 60:02d}Z "
            f"{rng.choice(levels)} service={service} method=GET path={rng.choice(paths)} "
            f"status={rng.choice([200] * 9 + [304, 404])} duration_ms={rng.randint(1, 180)}"
        )
    return ("\n".join(out) + "\n").encode()


def main(root):
    # photos
    albums = {
        "albums/2026/alps-trip": ["glacier-lake", "summit-sunrise", "mountain-hut", "valley-fog", "pine-forest", "alpine-meadow"],
        "albums/2026/city-walks": ["old-bridge", "harbour-lights", "market-square", "rooftops"],
        "albums/2025/coastline": ["lighthouse", "tide-pools", "cliff-path"],
    }
    n = 0
    for album, names in albums.items():
        for name in names:
            w, h = rng.choice([(2400, 1600), (2000, 1333), (1600, 2400), (2048, 1365)])
            photo(f"{root}/photos/{album}/{name}.jpg", w, h, PALETTES[n % len(PALETTES)])
            n += 1
    for i, name in enumerate(["glacier-lake", "summit-sunrise", "old-bridge", "lighthouse"]):
        photo(f"{root}/photos/thumbnails/{name}-320.jpg", 320, 213, PALETTES[i], quality=80)
    write(f"{root}/photos/raw/DSC_0412.dng", random_bytes(6_400_000))
    write(f"{root}/photos/raw/DSC_0413.dng", random_bytes(6_150_000))
    write(f"{root}/photos/raw/DSC_0419.dng", random_bytes(5_870_000))
    write(f"{root}/photos/videos/timelapse-clouds.mp4", random_bytes(9_800_000))
    write(
        f"{root}/photos/albums/2026/alps-trip/README.md",
        "# Alps trip 2026\n\nSelected shots from the summer hike. Originals are in raw/.\n",
    )
    write(
        f"{root}/photos/albums/2026/alps-trip/metadata.json",
        json.dumps(
            {"album": "Alps trip", "year": 2026, "photos": albums["albums/2026/alps-trip"], "camera": "Mirrorless 24MP"},
            indent=2,
        ),
    )

    # website
    site = f"{root}/website"
    page = """<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>{title} - Example Outdoor Co.</title>
  <link rel="stylesheet" href="/assets/css/site.css">
</head>
<body>
  <header><img src="/assets/img/logo.svg" alt="Example Outdoor Co."><nav><a href="/">Home</a> <a href="/about/">About</a> <a href="/blog/">Blog</a></nav></header>
  <main><h1>{title}</h1><p>{body}</p></main>
  <script src="/assets/js/app.js"></script>
</body>
</html>
"""
    write(f"{site}/index.html", page.format(title="Welcome", body="Gear and guides for the outdoors."))
    write(f"{site}/about/index.html", page.format(title="About us", body="A small team of hikers and engineers."))
    write(f"{site}/blog/index.html", page.format(title="Blog", body="Stories from the trail."))
    for slug in ["packing-list-for-the-alps", "choosing-a-tent", "winter-layering-guide"]:
        write(f"{site}/blog/{slug}/index.html", page.format(title=slug.replace("-", " ").capitalize(), body="Lorem ipsum dolor sit amet. " * 40))
    write(f"{site}/404.html", page.format(title="Page not found", body="Sorry, we could not find that page."))
    css = "body{font-family:system-ui,sans-serif;margin:0;color:#1d2433}\n" + "".join(
        f".c{i}{{margin:{i % 8}px;padding:{i % 5}px;color:#{rng.randrange(0x1000000):06x}}}\n" for i in range(900)
    )
    write(f"{site}/assets/css/site.css", css)
    js = "(function(){'use strict';\n" + "".join(
        f"  function f{i}(a,b){{return a*{i}+b-{rng.randint(1, 99)};}}\n" for i in range(2500)
    ) + "})();\n"
    write(f"{site}/assets/js/app.js", js)
    write(f"{site}/assets/js/vendor.js", js * 3)
    write(
        f"{site}/assets/img/logo.svg",
        '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="32"><rect width="32" height="32" rx="6" fill="#2f6fde"/>'
        '<text x="40" y="22" font-family="sans-serif" font-size="16">Example</text></svg>\n',
    )
    photo(f"{site}/assets/img/hero.jpg", 1920, 1080, PALETTES[1])
    photo(f"{site}/assets/img/team.jpg", 1200, 800, PALETTES[2])
    write(f"{site}/robots.txt", "User-agent: *\nAllow: /\n")
    write(f"{site}/sitemap.xml", '<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://www.example.com/</loc></url></urlset>\n')

    # backups
    for day in [1, 8, 15]:
        write(f"{root}/backups/postgres/orders-db-2026-10-{day:02d}.sql.gz", random_bytes(rng.randint(3_000_000, 4_500_000)))
    for day in [10, 11, 12, 13, 14, 15]:
        write(f"{root}/backups/postgres/wal/0000000100000004000000{day:02X}.gz", random_bytes(rng.randint(400_000, 900_000)))
    write(f"{root}/backups/configs/etc-2026-10-15.tar.zst", random_bytes(820_000))
    write(f"{root}/backups/configs/k8s-manifests-2026-10-15.tar.zst", random_bytes(310_000))
    write(f"{root}/backups/manifest.json", json.dumps({"retention": "30d", "schedule": "daily 02:00 UTC", "encrypted": True}, indent=2))

    # logs
    for service in ["web", "api", "worker"]:
        for day in [13, 14, 15, 16]:
            write(
                f"{root}/logs/{service}/2026/10/{day:02d}/{service}-{day:02d}.log.gz",
                gzip.compress(log_lines(service, day, rng.randint(2000, 6000)), mtime=0),
            )

    # analytics
    rows = ["date,region,visits,signups,revenue"]
    for d in range(1, 31):
        for region in ["north", "south", "east", "west"]:
            rows.append(f"2026-09-{d:02d},{region},{rng.randint(800, 4000)},{rng.randint(10, 90)},{rng.randint(1000, 9000)}.{rng.randint(0, 99):02d}")
    write(f"{root}/analytics/reports/2026-09/traffic-by-region.csv", "\n".join(rows) + "\n")
    write(f"{root}/analytics/reports/2026-09/summary.json", json.dumps({"month": "2026-09", "visits": 412_330, "signups": 6_212}, indent=2))
    write(f"{root}/analytics/datasets/events-2026-10.parquet", random_bytes(2_400_000))
    write(f"{root}/analytics/notebooks/churn-analysis.ipynb", json.dumps({"cells": [], "metadata": {}, "nbformat": 4, "nbformat_minor": 5}))


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "demo-files")
