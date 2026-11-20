"""Generate golden.json for gobi/h3's tests from h3-py.

h3-py wraps the H3 C library, so these values are independent of the
pure-Go h3-go that gobi/h3 is built on. Rerun when bumping either:

    conda run -n py313 python gen_golden.py [path/to/h3_agg.parquet]

The optional parquet (benchmarks/fixtures/h3_agg.parquet, with lat /
lon / h3_res8 columns) adds a sample of real coordinates; its stored
h3_res8 values are cross-checked here too.
"""

from __future__ import annotations

import json
import random
import sys
from pathlib import Path

import h3
from h3.api import basic_int as h3i

OUT = Path(__file__).parent / "golden.json"


def points() -> list[tuple[float, float]]:
    rng = random.Random(20261120)
    pts: list[tuple[float, float]] = [
        (90.0, 0.0), (-90.0, 0.0), (89.9999, 45.0), (-89.9999, -135.0),
        (0.0, 0.0), (0.0, 180.0), (0.0, -180.0), (10.0, 179.9999),
        (10.0, -179.9999), (-33.8688, 151.2093), (47.6062, -122.3321),
        (51.4779, -0.0015), (64.1466, -21.9426), (-77.85, 166.67),
    ]
    pts += [(rng.uniform(-90, 90), rng.uniform(-180, 180)) for _ in range(250)]
    if len(sys.argv) > 1:
        import pyarrow.parquet as pq

        t = pq.read_table(sys.argv[1], columns=["lat", "lon", "h3_res8"])
        lat, lon, cell = (t.column(c).to_pylist() for c in ("lat", "lon", "h3_res8"))
        idx = rng.sample(range(len(lat)), 250)
        for i in idx:
            assert h3i.latlng_to_cell(lat[i], lon[i], 8) == cell[i], "fixture h3_res8 disagrees"
            pts.append((lat[i], lon[i]))
    return pts


def main() -> None:
    pts = points()
    cells_rows = [
        {"lat": lat, "lng": lng, "cells": [h3i.latlng_to_cell(lat, lng, r) for r in range(16)]}
        for lat, lng in pts
    ]

    # Cell-level functions, on res 0 / 5 / 8 / 15 cells of the first
    # 50 points (poles and the antimeridian included).
    cells = []
    for row in cells_rows[:50]:
        for r in (0, 5, 8, 15):
            c = row["cells"][r]
            lat, lng = h3i.cell_to_latlng(c)
            cells.append({
                "cell": c,
                "res": h3i.get_resolution(c),
                "parents": [h3i.cell_to_parent(c, p) for p in range(r + 1)],
                "lat": lat,
                "lng": lng,
                "boundary": [list(v) for v in h3i.cell_to_boundary(c)],
                "disk1": sorted(h3i.grid_disk(c, 1)),
                "disk2": sorted(h3i.grid_disk(c, 2)) if r in (5, 8) else None,
                "str": h3.int_to_str(c),
            })

    invalid = [0, 1, 0x7FFFFFFFFFFFFFFF, 0x08FFFFFFFFFFFFFF, 0x1234567890ABCDEF]
    for c in invalid:
        assert not h3i.is_valid_cell(c)

    OUT.write_text(json.dumps({
        "h3_py": h3.__version__,
        "points": cells_rows,
        "cells": cells,
        "invalid": invalid,
    }, separators=(",", ":")))
    print(f"wrote {OUT} ({OUT.stat().st_size:,} bytes): "
          f"{len(cells_rows)} points, {len(cells)} cells")


if __name__ == "__main__":
    main()
