import { forwardRef, useEffect, useImperativeHandle, useRef, useState, type PointerEvent as ReactPointerEvent } from "react";
import { Eraser, Trash2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import i18n from "../i18n";
import { applyMaskStamp, blankMask, maskHasEdits } from "../lib/imageMask";

export type MaskPainterHandle = {
  maskBlob: () => Promise<Blob | null>;
  clear: () => void;
};

type Props = {
  url: string;
  disabled?: boolean;
  onEditsChange?: (active: boolean) => void;
};

const SIZES = [
  { label: "thin", radius: 12 },
  { label: "medium", radius: 28 },
  { label: "thick", radius: 56 },
] as const;

function pngMask(image: HTMLImageElement, alpha: Uint8Array): Promise<Blob> {
  const canvas = document.createElement("canvas");
  canvas.width = image.naturalWidth;
  canvas.height = image.naturalHeight;
  const ctx = canvas.getContext("2d");
  if (!ctx) return Promise.reject(new Error(i18n.t("keys.shared.mask.exportUnavailable")));
  ctx.drawImage(image, 0, 0);
  const pixels = ctx.getImageData(0, 0, canvas.width, canvas.height);
  for (let i = 0; i < alpha.length; i++) {
    if (alpha[i] === 0) pixels.data[i * 4 + 3] = 0;
  }
  ctx.putImageData(pixels, 0, 0);
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error(i18n.t("keys.shared.mask.exportFailed")))), "image/png");
  });
}

export const MaskPainter = forwardRef<MaskPainterHandle, Props>(function MaskPainter({ url, disabled, onEditsChange }, ref) {
  const { t } = useTranslation();
  const viewRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const imageRef = useRef<HTMLImageElement | null>(null);
  const alphaRef = useRef<Uint8Array | null>(null);
  const drawing = useRef(false);
  const last = useRef<{ x: number; y: number } | null>(null);
  const toolRef = useRef<"brush" | "eraser">("brush");
  const editsChangeRef = useRef(onEditsChange);
  editsChangeRef.current = onEditsChange;
  const radiusRef = useRef(28);
  const [tool, setTool] = useState<"brush" | "eraser">("brush");
  const [radius, setRadius] = useState(28);
  const [box, setBox] = useState({ w: 0, h: 0 });
  const [natural, setNatural] = useState({ w: 0, h: 0 });

  toolRef.current = tool;
  radiusRef.current = radius;

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const measure = () => setBox({ w: view.clientWidth, h: view.clientHeight });
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(view);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    const image = new Image();
    image.onload = () => {
      imageRef.current = image;
      alphaRef.current = blankMask(image.naturalWidth, image.naturalHeight);
      editsChangeRef.current?.(false);
      setNatural({ w: image.naturalWidth, h: image.naturalHeight });
    };
    image.src = url;
    return () => {
      imageRef.current = null;
      alphaRef.current = null;
    };
  }, [url]);

  const frame = (() => {
    if (!box.w || !box.h || !natural.w || !natural.h) return null;
    const scale = Math.min(box.w / natural.w, box.h / natural.h);
    const width = Math.max(1, Math.floor(natural.w * scale));
    const height = Math.max(1, Math.floor(natural.h * scale));
    return {
      left: Math.floor((box.w - width) / 2),
      top: Math.floor((box.h - height) / 2),
      width,
      height,
    };
  })();

  const redraw = () => {
    const canvas = canvasRef.current;
    const image = imageRef.current;
    const alpha = alphaRef.current;
    if (!canvas || !image || !alpha || !frame) return;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, Math.floor(frame.width * dpr));
    canvas.height = Math.max(1, Math.floor(frame.height * dpr));
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
    const tint = ctx.createImageData(canvas.width, canvas.height);
    for (let y = 0; y < canvas.height; y++) {
      const sy = Math.min(image.naturalHeight - 1, (y * image.naturalHeight / canvas.height) | 0);
      for (let x = 0; x < canvas.width; x++) {
        const sx = Math.min(image.naturalWidth - 1, (x * image.naturalWidth / canvas.width) | 0);
        if (alpha[sy * image.naturalWidth + sx] !== 0) continue;
        const i = (y * canvas.width + x) * 4;
        tint.data[i] = 220;
        tint.data[i + 1] = 48;
        tint.data[i + 2] = 48;
        tint.data[i + 3] = 120;
      }
    }
    ctx.putImageData(tint, 0, 0);
    ctx.globalCompositeOperation = "destination-over";
    ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
    ctx.globalCompositeOperation = "source-over";
  };

  const clearMask = () => {
    const image = imageRef.current;
    if (!image) return;
    alphaRef.current = blankMask(image.naturalWidth, image.naturalHeight);
    redraw();
    editsChangeRef.current?.(false);
  };

  useImperativeHandle(ref, () => ({
    maskBlob: async () => {
      const image = imageRef.current;
      const alpha = alphaRef.current;
      if (!image || !alpha || !maskHasEdits(alpha)) return null;
      return pngMask(image, alpha);
    },
    clear: clearMask,
  }));

  useEffect(() => {
    redraw();
    // frame size and the loaded image are the only reasons to repaint
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [frame?.left, frame?.top, frame?.width, frame?.height, natural.w, natural.h, url]);

  const imagePoint = (event: ReactPointerEvent<HTMLCanvasElement>) => {
    const canvas = canvasRef.current;
    const image = imageRef.current;
    if (!canvas || !image) return null;
    const rect = canvas.getBoundingClientRect();
    if (!rect.width || !rect.height) return null;
    return {
      x: ((event.clientX - rect.left) / rect.width) * image.naturalWidth,
      y: ((event.clientY - rect.top) / rect.height) * image.naturalHeight,
    };
  };

  const stamp = (from: { x: number; y: number }, to: { x: number; y: number }) => {
    const image = imageRef.current;
    const alpha = alphaRef.current;
    const canvas = canvasRef.current;
    if (!image || !alpha || !canvas) return;
    const rect = canvas.getBoundingClientRect();
    const radiusPx = radiusRef.current * (image.naturalWidth / rect.width);
    applyMaskStamp(alpha, image.naturalWidth, image.naturalHeight, {
      x0: from.x,
      y0: from.y,
      x1: to.x,
      y1: to.y,
      radius: radiusPx,
      erase: toolRef.current === "eraser",
    });
  };

  const maskTools = (
    <div className="jc-mask-tools">
      <button type="button" className={tool === "brush" ? "is-on" : ""} title={t("keys.shared.mask.brushTitle")} onClick={() => setTool("brush")}>{t("keys.shared.mask.brush")}</button>
      <button type="button" className={tool === "eraser" ? "is-on" : ""} aria-label={t("keys.shared.mask.eraser")} onClick={() => setTool("eraser")}>
        <Eraser size={14} />
      </button>
      {SIZES.map((item) => (
        <button key={item.label} type="button" className={radius === item.radius ? "is-on" : ""} onClick={() => setRadius(item.radius)}>
          {t(`keys.shared.mask.${item.label}`)}
        </button>
      ))}
      <button
        type="button"
        aria-label={t("keys.shared.mask.clear")}
        onClick={clearMask}
      >
        <Trash2 size={14} />
      </button>
    </div>
  );
  return (
    <div className="jc-mask-stage">
      {maskTools}
      <div className="jc-mask-view" ref={viewRef}>
        {frame && (
          <div className="jc-mask-frame" style={{ left: frame.left, top: frame.top, width: frame.width, height: frame.height }}>
            <canvas
              ref={canvasRef}
              aria-label={t("keys.shared.mask.canvas")}
              style={{ pointerEvents: disabled ? "none" : "auto" }}
              onPointerDown={(event) => {
                if (disabled) return;
                const point = imagePoint(event);
                if (!point) return;
                event.currentTarget.setPointerCapture(event.pointerId);
                drawing.current = true;
                last.current = point;
                stamp(point, point);
                redraw();
              }}
              onPointerMove={(event) => {
                if (!drawing.current || disabled) return;
                const point = imagePoint(event);
                const prev = last.current;
                if (!point || !prev) return;
                stamp(prev, point);
                last.current = point;
                redraw();
              }}
              onPointerUp={() => {
                drawing.current = false;
                const alpha = alphaRef.current;
                if (alpha) editsChangeRef.current?.(maskHasEdits(alpha));
                last.current = null;
              }}
            />
          </div>
        )}
      </div>
    </div>
  );
});
