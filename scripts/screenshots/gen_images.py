#!/usr/bin/env python3
"""Generates the demo photos of the image bucket of the README screenshots.

Usage: gen_images.py OUTPUT_DIR

Photo-like scenes drawn with Pillow (required: the script exits with status 2
without it), deterministic (fixed seeds) so that the screenshots are
reproducible:

  landscapes/alpine-lake.jpg     4000x3000 (12 megapixels: 28 variants)
  landscapes/northern-lights.jpg 2400x1600
  landscapes/desert-dunes.jpg    1280x853
  city/night-lights.jpg          1920x1280
  brand/sticker.png              1024x1024 with transparency
"""

import math
import os
import random
import sys
from itertools import pairwise

try:
    from PIL import Image, ImageChops, ImageDraw, ImageFilter
except ImportError:  # pragma: no cover
    print("gen_images: Pillow is not installed (pip install pillow)", file=sys.stderr)
    sys.exit(2)


def lerp(a, b, t):
    return tuple(round(a[i] + (b[i] - a[i]) * t) for i in range(len(a)))


def vertical(width, height, stops):
    """A vertical gradient through the (position, colour) stops."""
    column = Image.new("RGB", (1, height))
    px = column.load()
    for y in range(height):
        t = y / max(1, height - 1)
        for (p0, c0), (p1, c1) in pairwise(stops):
            if p0 <= t <= p1:
                px[0, y] = lerp(c0, c1, (t - p0) / max(1e-9, p1 - p0))
                break
    return column.resize((width, height), Image.Resampling.BILINEAR)


def glow(size, box, colour, blur, opacity):
    """A soft light (a blurred ellipse) as an RGBA layer."""
    layer = Image.new("RGBA", size, colour + (0,))
    mask = Image.new("L", size, 0)
    ImageDraw.Draw(mask).ellipse(box, fill=int(255 * opacity))
    layer.putalpha(mask.filter(ImageFilter.GaussianBlur(blur)))
    return layer


def ridge(rng, width, base, roughness, points=257):
    """A mountain ridge: midpoint displacement between two heights."""
    heights = [0.0] * points
    heights[0] = base + rng.uniform(-roughness, roughness)
    heights[-1] = base + rng.uniform(-roughness, roughness)
    step, scale = points - 1, roughness
    while step > 1:
        half = step // 2
        for i in range(half, points - 1, step):
            heights[i] = (heights[i - half] + heights[i + half]) / 2 + rng.uniform(
                -scale, scale
            )
        step, scale = half, scale * 0.55
    return [(width * i / (points - 1), h) for i, h in enumerate(heights)]


def grain(img, amount):
    """Film grain, so that the files have the size of real photos."""
    noise = Image.effect_noise(img.size, 40).convert("RGB")
    return ImageChops.blend(img, ImageChops.overlay(img, noise), amount)


def alpine_lake(path):
    rng = random.Random(41)
    w, h = 4000, 3000
    horizon = int(h * 0.62)
    sky = vertical(
        w,
        horizon,
        [
            (0, (24, 38, 84)),
            (0.45, (92, 92, 160)),
            (0.8, (238, 150, 128)),
            (1, (255, 206, 150)),
        ],
    )
    img = Image.new("RGB", (w, h))
    img.paste(sky, (0, 0))
    img = img.convert("RGBA")
    img.alpha_composite(
        glow(
            (w, h),
            (w * 0.52, horizon - 520, w * 0.82, horizon + 260),
            (255, 226, 170),
            180,
            0.85,
        )
    )
    img.alpha_composite(
        glow(
            (w, h),
            (w * 0.62, horizon - 190, w * 0.70, horizon - 80),
            (255, 248, 225),
            40,
            0.95,
        )
    )
    draw = ImageDraw.Draw(img)
    # Thin clouds lit from below.
    for _ in range(14):
        cx, cy = rng.uniform(0, w), rng.uniform(h * 0.12, horizon * 0.7)
        cw, ch = rng.uniform(500, 1300), rng.uniform(30, 70)
        img.alpha_composite(
            glow(
                (w, h),
                (cx - cw / 2, cy, cx + cw / 2, cy + ch),
                (255, 190, 170),
                30,
                0.35,
            )
        )
    # Mountain ranges, lighter with the haze of the distance; the light of
    # the dawn on the snow of the farthest one.
    layers = [
        (horizon - 520, 420, (196, 150, 186), (150, 128, 176)),
        (horizon - 330, 220, (92, 92, 142), (126, 112, 160)),
        (horizon - 170, 110, (52, 58, 96), (78, 78, 120)),
        (horizon - 50, 40, (26, 34, 52), (34, 42, 62)),
    ]
    for base, rough, top, bottom in layers:
        pts = ridge(rng, w, base, rough)
        mask = Image.new("L", (w, h), 0)
        ImageDraw.Draw(mask).polygon(pts + [(w, horizon), (0, horizon)], fill=255)
        fill = Image.new("RGBA", (w, h))
        fill.paste(vertical(w, horizon, [(0, top), (0.6, top), (1, bottom)]), (0, 0))
        fill.putalpha(mask)
        img.alpha_composite(fill)
    draw = ImageDraw.Draw(img)
    # The lake: the scene mirrored, darker and rippled.
    scene = img.crop((0, 0, w, horizon)).transpose(Image.Transpose.FLIP_TOP_BOTTOM)
    scene = scene.resize((w, h - horizon)).filter(ImageFilter.GaussianBlur(6))
    dark = Image.new("RGBA", scene.size, (14, 24, 46, 255))
    lake = Image.blend(scene, dark, 0.38)
    ld = ImageDraw.Draw(lake)
    for _ in range(260):
        y = rng.uniform(0, h - horizon)
        x = rng.uniform(0, w)
        length = rng.uniform(40, 260) * (0.4 + y / (h - horizon))
        ld.line((x, y, x + length, y), fill=(255, 214, 190, 30), width=3)
    img.paste(lake, (0, horizon))
    # The near shore.
    shore = ridge(rng, w, h - 140, 70)
    draw.polygon(shore + [(w, h), (0, h)], fill=(16, 22, 30, 255))
    out = grain(img.convert("RGB"), 0.18)
    out.save(path, quality=90, optimize=True)


def northern_lights(path):
    rng = random.Random(7)
    w, h = 2400, 1600
    img = vertical(
        w, h, [(0, (4, 8, 26)), (0.6, (10, 30, 52)), (1, (16, 44, 58))]
    ).convert("RGBA")
    draw = ImageDraw.Draw(img)
    for _ in range(900):
        x, y, r = rng.uniform(0, w), rng.uniform(0, h * 0.75), rng.choice((1, 1, 1, 2))
        draw.ellipse(
            (x - r, y - r, x + r, y + r), fill=(255, 255, 255, rng.randint(90, 230))
        )
    for colour, offset, width, blur in [
        ((56, 239, 158), 0, 140, 60),
        ((40, 200, 220), 90, 90, 50),
        ((160, 100, 240), -120, 70, 70),
        ((120, 255, 190), 30, 30, 18),
    ]:
        band = Image.new("RGBA", (w, h), colour + (0,))
        mask = Image.new("L", (w, h), 0)
        md = ImageDraw.Draw(mask)
        pts = ridge(random.Random(3), w, h * 0.38 + offset, 260, points=65)
        for (x0, y0), (x1, y1) in pairwise(pts):
            md.line((x0, y0, x1, y1), fill=200, width=width)
            for k in range(0, 360, 30):
                md.line((x0, y0 + k, x1, y1 + k), fill=max(0, 120 - k // 3), width=6)
        band.putalpha(mask.filter(ImageFilter.GaussianBlur(blur)))
        img.alpha_composite(band)
    # A forest of firs on the horizon.
    ground = h - 230
    for i in range(160):
        x = rng.uniform(-40, w + 40)
        th = rng.uniform(120, 360)
        tw = th * rng.uniform(0.28, 0.4)
        base = ground + rng.uniform(0, 60)
        draw.polygon(
            [(x, base - th), (x - tw / 2, base), (x + tw / 2, base)],
            fill=(3, 8, 14, 255),
        )
    draw.rectangle((0, ground + 40, w, h), fill=(3, 8, 14, 255))
    grain(img.convert("RGB"), 0.12).save(path, quality=90, optimize=True)


def desert_dunes(path):
    rng = random.Random(19)
    w, h = 1280, 853
    img = vertical(
        w, h, [(0, (88, 150, 214)), (0.42, (250, 200, 160)), (1, (250, 214, 170))]
    ).convert("RGBA")
    img.alpha_composite(
        glow((w, h), (w * 0.12, h * 0.16, w * 0.34, h * 0.48), (255, 240, 200), 70, 0.8)
    )
    draw = ImageDraw.Draw(img)
    for k, (top, light, shade) in enumerate(
        [
            (h * 0.46, (232, 160, 104), (196, 116, 78)),
            (h * 0.56, (226, 140, 84), (176, 92, 58)),
            (h * 0.68, (214, 122, 70), (150, 72, 44)),
            (h * 0.82, (196, 104, 58), (122, 56, 36)),
        ]
    ):
        phase, amp, period = rng.uniform(0, 6.28), 40 + 18 * k, rng.uniform(380, 620)
        pts = [
            (x, top + amp * math.sin(x / period * 6.28 + phase))
            for x in range(0, w + 8, 8)
        ]
        draw.polygon(pts + [(w, h), (0, h)], fill=light + (255,))
        # The shaded side of the crest.
        crest = max(pts, key=lambda p: -p[1])
        shadow = [(x, y) for x, y in pts if x >= crest[0]]
        if len(shadow) > 2:
            poly = shadow + [(w, h), (crest[0] + (w - crest[0]) * 0.2, h)]
            layer = Image.new("RGBA", (w, h), shade + (0,))
            mask = Image.new("L", (w, h), 0)
            ImageDraw.Draw(mask).polygon(poly, fill=150)
            layer.putalpha(mask.filter(ImageFilter.GaussianBlur(18)))
            img.alpha_composite(layer)
    grain(img.convert("RGB"), 0.2).save(path, quality=90, optimize=True)


def night_lights(path):
    rng = random.Random(23)
    w, h = 1920, 1280
    img = vertical(
        w, h, [(0, (12, 10, 36)), (0.5, (40, 18, 66)), (1, (10, 14, 30))]
    ).convert("RGBA")
    palette = [
        (255, 170, 60),
        (255, 90, 120),
        (90, 170, 255),
        (190, 120, 255),
        (255, 220, 140),
        (80, 230, 210),
    ]
    for blur, count, size, alpha in [
        (30, 40, (90, 180), 0.35),
        (10, 70, (40, 110), 0.55),
        (3, 90, (14, 40), 0.8),
    ]:
        layer = Image.new("RGBA", (w, h), (0, 0, 0, 0))
        ld = ImageDraw.Draw(layer)
        for _ in range(count):
            x, y = rng.uniform(0, w), rng.gauss(h * 0.55, h * 0.18)
            r = rng.uniform(*size)
            c = rng.choice(palette)
            ld.ellipse(
                (x - r, y - r, x + r, y + r),
                fill=c + (int(255 * alpha * rng.uniform(0.6, 1)),),
            )
        img.alpha_composite(layer.filter(ImageFilter.GaussianBlur(blur)))
    grain(img.convert("RGB"), 0.15).save(path, quality=90, optimize=True)


def sticker(path):
    """A badge on a transparent background, with a soft shadow."""
    s = 1024
    img = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    shadow = Image.new("RGBA", (s, s), (20, 20, 60, 0))
    mask = Image.new("L", (s, s), 0)
    ImageDraw.Draw(mask).rounded_rectangle((150, 190, 890, 930), 190, fill=120)
    shadow.putalpha(mask.filter(ImageFilter.GaussianBlur(40)))
    img.alpha_composite(shadow)
    face = vertical(
        s, s, [(0, (99, 102, 241)), (0.55, (76, 110, 245)), (1, (34, 211, 238))]
    ).convert("RGBA")
    shape = Image.new("L", (s, s), 0)
    ImageDraw.Draw(shape).rounded_rectangle((132, 132, 892, 892), 180, fill=255)
    face.putalpha(shape)
    img.alpha_composite(face)
    draw = ImageDraw.Draw(img)
    # Three stacked "objects", the mark of a store.
    for i, y in enumerate((330, 470, 610)):
        draw.rounded_rectangle(
            (302, y, 722, y + 96), 48, fill=(255, 255, 255, 235 - i * 40)
        )
        draw.ellipse((622, y + 30, 658, y + 66), fill=(76, 110, 245, 255))
    img.save(path, optimize=True)


def main(root):
    jobs = [
        ("landscapes/alpine-lake.jpg", alpine_lake),
        ("landscapes/northern-lights.jpg", northern_lights),
        ("landscapes/desert-dunes.jpg", desert_dunes),
        ("city/night-lights.jpg", night_lights),
        ("brand/sticker.png", sticker),
    ]
    for name, make in jobs:
        path = os.path.join(root, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        make(path)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: gen_images.py OUTPUT_DIR", file=sys.stderr)
        sys.exit(2)
    main(sys.argv[1])
