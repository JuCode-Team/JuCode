import { useEffect, useRef, useState } from "react";

const VERTEX_SHADER = `
attribute vec2 a_position;

void main() {
  gl_Position = vec4(a_position, 0.0, 1.0);
}
`;

const FRAGMENT_SHADER = `
precision highp float;

uniform vec2 u_resolution;
uniform vec2 u_pointer;
uniform float u_dpr;
uniform float u_hover;
uniform float u_time;
uniform float u_light;

void main() {
  vec2 cssResolution = u_resolution / u_dpr;
  vec2 point = gl_FragCoord.xy / u_dpr;
  vec2 uv = point / cssResolution;

  float stepSize = cssResolution.x < 560.0 ? 10.0 : 11.0;
  vec2 cell = abs(fract(point / stepSize) - 0.5) * stepSize;

  float carrier = 0.5 + sin(uv.x * 8.4 + u_time * 0.72) * 0.17;
  float secondary = 0.52 + sin(uv.x * 5.2 - u_time * 0.5328) * 0.12;
  float distanceToWave = min(abs(uv.y - carrier), abs(uv.y - secondary));

  float pointerDistance = distance(uv, u_pointer);
  float pointerInfluence = (1.0 - smoothstep(0.0, 0.2, pointerDistance)) * u_hover;
  distanceToWave -= pointerInfluence * 0.07;

  float strength = clamp(1.0 - distanceToWave / 0.17, 0.0, 1.0);

  // Preserve the original Canvas look: compact, hard-edged square pixels.
  float halfSize = 0.65 + strength * 0.75 + pointerInfluence * 0.55;
  float squareDistance = max(cell.x, cell.y);
  float dotMask = 1.0 - smoothstep(halfSize, halfSize + 0.15, squareDistance);
  float alpha = min(1.0, 0.16 + pow(strength, 0.72) * 0.84 + pointerInfluence * 0.12);
  alpha *= dotMask * step(0.06, strength);

  vec3 purple = mix(vec3(0.592, 0.404, 0.933), vec3(0.396, 0.251, 0.722), u_light);
  vec3 blue = mix(vec3(0.537, 0.808, 1.0), vec3(0.224, 0.486, 0.635), u_light);
  vec3 color = mix(purple, blue, step(0.52, uv.x));
  // The context uses premultipliedAlpha: true; emitting straight alpha here is
  // undefined behavior and renders as a washed-out smear on Windows/ANGLE.
  gl_FragColor = vec4(color * alpha, alpha);
}
`;

function compileShader(gl: WebGLRenderingContext, type: number, source: string) {
  const shader = gl.createShader(type);
  if (!shader) return null;
  gl.shaderSource(shader, source);
  gl.compileShader(shader);
  if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
    console.warn("SignalRaster shader compile failed:", gl.getShaderInfoLog(shader));
    gl.deleteShader(shader);
    return null;
  }
  return shader;
}

function setupCanvasFallback(canvas: HTMLCanvasElement) {
  const ctx = canvas.getContext("2d");
  if (!ctx) return undefined;

  const draw = () => {
    const rect = canvas.getBoundingClientRect();
    const ratio = Math.min(window.devicePixelRatio || 1, 1.5);
    const width = Math.max(1, rect.width);
    const height = Math.max(1, rect.height);
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
    ctx.clearRect(0, 0, width, height);

    const step = width < 560 ? 10 : 11;
    for (let y = 0; y <= height; y += step) {
      for (let x = 0; x <= width; x += step) {
        const nx = x / width;
        const ny = y / height;
        const carrier = 0.5 + Math.sin(nx * 8.4) * 0.17;
        const secondary = 0.52 + Math.sin(nx * 5.2) * 0.12;
        const strength = Math.max(0, 1 - Math.min(Math.abs(ny - carrier), Math.abs(ny - secondary)) / 0.17);
        if (strength <= 0.06) continue;
        const alpha = 0.08 + strength * 0.78;
        ctx.fillStyle = nx < 0.52
          ? `rgba(151, 103, 238, ${alpha})`
          : `rgba(137, 206, 255, ${alpha * 0.9})`;
        ctx.fillRect(x, y, 1 + strength * 1.35, 1 + strength * 1.35);
      }
    }
    canvas.dataset.ready = "true";
  };

  draw();
  window.addEventListener("resize", draw);
  return () => window.removeEventListener("resize", draw);
}

export default function SignalRaster({ className = "" }: { className?: string }) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  // Once a canvas has been asked for a WebGL context it can never hand out a
  // 2D one, so falling back requires remounting the canvas (key={mode}).
  const [mode, setMode] = useState<"webgl" | "canvas2d">("webgl");

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;

    if (mode === "canvas2d") return setupCanvasFallback(canvas);

    const fallBack = () => setMode("canvas2d");

    const gl = canvas.getContext("webgl", {
      alpha: true,
      antialias: false,
      depth: false,
      powerPreference: "low-power",
      premultipliedAlpha: true,
    });
    if (!gl) return fallBack();

    const vertex = compileShader(gl, gl.VERTEX_SHADER, VERTEX_SHADER);
    const fragment = compileShader(gl, gl.FRAGMENT_SHADER, FRAGMENT_SHADER);
    if (!vertex || !fragment) return fallBack();

    const program = gl.createProgram();
    if (!program) return fallBack();
    gl.attachShader(program, vertex);
    gl.attachShader(program, fragment);
    gl.linkProgram(program);
    gl.deleteShader(vertex);
    gl.deleteShader(fragment);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
      console.warn("SignalRaster program link failed:", gl.getProgramInfoLog(program));
      gl.deleteProgram(program);
      return fallBack();
    }

    const buffer = gl.createBuffer();
    if (!buffer) return fallBack();
    gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);

    gl.useProgram(program);
    const position = gl.getAttribLocation(program, "a_position");
    gl.enableVertexAttribArray(position);
    gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);

    const resolutionUniform = gl.getUniformLocation(program, "u_resolution");
    const pointerUniform = gl.getUniformLocation(program, "u_pointer");
    const dprUniform = gl.getUniformLocation(program, "u_dpr");
    const hoverUniform = gl.getUniformLocation(program, "u_hover");
    const timeUniform = gl.getUniformLocation(program, "u_time");
    const lightUniform = gl.getUniformLocation(program, "u_light");

    let ratio = 1;
    let rect = canvas.getBoundingClientRect();
    let targetX = 0.72;
    let targetY = 0.5;
    let currentX = targetX;
    let currentY = targetY;
    let targetHover = 0;
    let currentHover = 0;
    let frame = 0;
    let running = false;
    let visible = true;
    let ready = false;
    let motionTime = 0;
    let lastFrameTime = 0;
    let scrollBoost = 0;
    let lastScrollY = window.scrollY;
    let lightTheme = document.documentElement.classList.contains("light") ? 1 : 0;
    const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    const updateRect = () => {
      rect = canvas.getBoundingClientRect();
    };

    const resize = () => {
      updateRect();
      // A background dot field does not benefit from full Retina resolution.
      // Capping DPR keeps fill-rate stable on 3x and 4x displays.
      ratio = Math.min(window.devicePixelRatio || 1, 1.5);
      const width = Math.max(1, Math.round(rect.width * ratio));
      const height = Math.max(1, Math.round(rect.height * ratio));
      if (canvas.width !== width || canvas.height !== height) {
        canvas.width = width;
        canvas.height = height;
        gl.viewport(0, 0, width, height);
      }
    };

    const draw = (time: number) => {
      currentX += (targetX - currentX) * 0.16;
      currentY += (targetY - currentY) * 0.16;
      currentHover += (targetHover - currentHover) * 0.12;

      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.uniform2f(resolutionUniform, canvas.width, canvas.height);
      gl.uniform2f(pointerUniform, currentX, currentY);
      gl.uniform1f(dprUniform, ratio);
      gl.uniform1f(hoverUniform, currentHover);
      gl.uniform1f(timeUniform, reduceMotion ? 0 : time);
      gl.uniform1f(lightUniform, lightTheme);
      gl.drawArrays(gl.TRIANGLES, 0, 3);
      if (!ready) {
        ready = true;
        canvas.dataset.ready = "true";
      }
    };

    const render = (now: number) => {
      const delta = lastFrameTime === 0 ? 0 : Math.min(0.05, (now - lastFrameTime) / 1000);
      lastFrameTime = now;
      scrollBoost *= Math.exp(-delta * 4.8);
      motionTime += delta * (1 + scrollBoost * 2.6);
      draw(motionTime);
      if (running) frame = window.requestAnimationFrame(render);
    };

    const start = () => {
      if (running || reduceMotion || !visible || document.hidden) return;
      running = true;
      lastFrameTime = 0;
      frame = window.requestAnimationFrame(render);
    };

    const stop = () => {
      running = false;
      window.cancelAnimationFrame(frame);
    };

    const onPointerMove = (event: PointerEvent) => {
      const inside = event.clientX >= rect.left && event.clientX <= rect.right &&
        event.clientY >= rect.top && event.clientY <= rect.bottom;
      targetHover = inside ? 1 : 0;
      if (inside) {
        targetX = (event.clientX - rect.left) / rect.width;
        targetY = 1 - (event.clientY - rect.top) / rect.height;
      }
      if (reduceMotion) {
        currentX = targetX;
        currentY = targetY;
        currentHover = targetHover;
        draw(0);
      }
    };

    const onResize = () => {
      resize();
      draw(motionTime);
    };
    const onScroll = () => {
      const nextY = window.scrollY;
      const distance = Math.abs(nextY - lastScrollY);
      lastScrollY = nextY;
      scrollBoost = Math.min(3, scrollBoost + distance / 140);
      updateRect();
    };
    const onSignalPulse = () => {
      scrollBoost = Math.max(scrollBoost, 1.45);
    };
    const onVisibilityChange = () => document.hidden ? stop() : start();
    const onContextLost = (event: Event) => {
      // GPU driver resets (common on Windows) would otherwise leave the
      // canvas permanently blank; drop to the static 2D rendering instead.
      event.preventDefault();
      stop();
      fallBack();
    };
    const themeObserver = new MutationObserver(() => {
      lightTheme = document.documentElement.classList.contains("light") ? 1 : 0;
      if (reduceMotion) draw(0);
    });
    const observer = new IntersectionObserver(([entry]) => {
      visible = entry.isIntersecting;
      if (visible) start();
      else stop();
    }, { threshold: 0.01 });

    resize();
    draw(0);
    observer.observe(canvas);
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    window.addEventListener("resize", onResize);
    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("jucode:signal-pulse", onSignalPulse);
    window.addEventListener("pointermove", onPointerMove, { passive: true });
    document.addEventListener("visibilitychange", onVisibilityChange);
    canvas.addEventListener("webglcontextlost", onContextLost);
    start();

    return () => {
      stop();
      observer.disconnect();
      themeObserver.disconnect();
      window.removeEventListener("resize", onResize);
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("jucode:signal-pulse", onSignalPulse);
      window.removeEventListener("pointermove", onPointerMove);
      document.removeEventListener("visibilitychange", onVisibilityChange);
      canvas.removeEventListener("webglcontextlost", onContextLost);
      gl.deleteBuffer(buffer);
      gl.deleteProgram(program);
    };
  }, [mode]);

  return <canvas key={mode} ref={canvasRef} className={className} aria-hidden="true" />;
}
