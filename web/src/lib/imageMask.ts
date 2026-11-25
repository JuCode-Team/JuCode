export type MaskStamp = {
  x0: number;
  y0: number;
  x1: number;
  y1: number;
  radius: number;
  erase: boolean;
};

export function blankMask(width: number, height: number): Uint8Array {
  const alpha = new Uint8Array(width * height);
  alpha.fill(255);
  return alpha;
}

export function maskHasEdits(alpha: Uint8Array): boolean {
  for (let i = 0; i < alpha.length; i++) if (alpha[i] === 0) return true;
  return false;
}

function fillDisk(alpha: Uint8Array, width: number, height: number, x: number, y: number, radius: number, value: number) {
  const r = Math.max(radius, 0.5);
  const r2 = r * r;
  const x0 = Math.max(0, Math.floor(x - r));
  const x1 = Math.min(width - 1, Math.ceil(x + r));
  const y0 = Math.max(0, Math.floor(y - r));
  const y1 = Math.min(height - 1, Math.ceil(y + r));
  for (let yy = y0; yy <= y1; yy++) {
    for (let xx = x0; xx <= x1; xx++) {
      const dx = xx - x;
      const dy = yy - y;
      if (dx * dx + dy * dy <= r2) alpha[yy * width + xx] = value;
    }
  }
}

export function applyMaskStamp(alpha: Uint8Array, width: number, height: number, stamp: MaskStamp) {
  const value = stamp.erase ? 255 : 0;
  const dist = Math.hypot(stamp.x1 - stamp.x0, stamp.y1 - stamp.y0);
  const step = Math.max(1, stamp.radius / 3);
  const n = Math.max(1, Math.ceil(dist / step));
  for (let i = 0; i <= n; i++) {
    const t = i / n;
    fillDisk(
      alpha,
      width,
      height,
      stamp.x0 + (stamp.x1 - stamp.x0) * t,
      stamp.y0 + (stamp.y1 - stamp.y0) * t,
      stamp.radius,
      value,
    );
  }
}
