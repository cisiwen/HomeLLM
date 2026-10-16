"""A tiny local HTTP service around HunyuanVideo-Foley, for HomeLLM's "add sound to a clip".

HomeLLM (internal/home/soundgen.go) is its only client and runs on the same PC, so files are
passed by path rather than uploaded. It listens on 127.0.0.1 only.

  POST /generate {"video": "C:/.../in.mp4", "out": "C:/.../out.mp4", "prompt": "rain, bus engine",
                  "neg_prompt"?: "...", "steps"?: 50, "guidance"?: 4.5, "seed"?: 1}
       -> {"out": "...", "seconds": 12.3}
  POST /free  -> unloads the model so the GPU can be used by something else
  GET  /health -> {"ok": true, "loaded": false}

Run with the venv's python from the HunyuanVideo-Foley checkout (so its package imports):
  python foley_server.py --models D:/HomeAI/foley-models --port 8190
"""
import argparse
import gc
import json
import os
import random
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch
import torchaudio
from loguru import logger

from hunyuanvideo_foley.utils.feature_utils import feature_process
from hunyuanvideo_foley.utils.media_utils import merge_audio_video
from hunyuanvideo_foley.utils.model_utils import denoise_process, load_model

# One job at a time: there's one GPU, and HomeLLM serialises jobs anyway.
_lock = threading.Lock()
_model = None  # (model_dict, cfg)
_args = None


def _load():
    global _model
    if _model is None:
        t = time.time()
        _model = load_model(
            _args.models,
            os.path.join(_args.models, "config_xl.yaml"),
            torch.device("cuda:0"),
            enable_offload=True,  # loads each sub-model only for its own stage
            model_size="xl",
        )
        logger.info(f"model loaded in {time.time() - t:.1f}s")
    return _model


def _free():
    global _model
    _model = None
    gc.collect()
    if torch.cuda.is_available():
        torch.cuda.empty_cache()


def _generate(req):
    video, out, prompt = req["video"], req["out"], req.get("prompt", "")
    if not os.path.isfile(video):
        raise ValueError(f"no such video: {video}")
    # NumPy only takes 32-bit seeds; HomeLLM's are wider.
    seed = int(req.get("seed") or 1) % 2**32
    random.seed(seed)
    np.random.seed(seed)
    torch.manual_seed(seed)

    model_dict, cfg = _load()
    t = time.time()
    visual_feats, text_feats, seconds = feature_process(video, prompt, model_dict, cfg, neg_prompt=req.get("neg_prompt"))
    audio, rate = denoise_process(
        visual_feats, text_feats, seconds, model_dict, cfg,
        guidance_scale=float(req.get("guidance") or 4.5),
        num_inference_steps=int(req.get("steps") or 50),
    )
    with tempfile.TemporaryDirectory() as tmp:
        wav = os.path.join(tmp, "audio.wav")
        torchaudio.save(wav, audio[0], rate)
        merge_audio_video(wav, video, out)
    took = time.time() - t
    logger.info(f"sound added in {took:.1f}s -> {out}")
    return {"out": out, "seconds": round(took, 1)}


class Handler(BaseHTTPRequestHandler):
    def _reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/health":
            return self._reply(200, {"ok": True, "loaded": _model is not None})
        self._reply(404, {"error": "not found"})

    def do_POST(self):
        try:
            length = int(self.headers.get("Content-Length") or 0)
            req = json.loads(self.rfile.read(length) or b"{}")
            with _lock:
                if self.path == "/free":
                    _free()
                    return self._reply(200, {"ok": True})
                if self.path == "/generate":
                    return self._reply(200, _generate(req))
            self._reply(404, {"error": "not found"})
        except Exception as e:  # report every failure to HomeLLM rather than dropping the connection
            logger.exception("request failed")
            self._reply(500, {"error": str(e)})

    def log_message(self, fmt, *args):
        logger.info(fmt % args)


def main():
    global _args
    p = argparse.ArgumentParser()
    p.add_argument("--models", required=True, help="directory with the XL weights and config_xl.yaml")
    p.add_argument("--port", type=int, default=8190)
    _args = p.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", _args.port), Handler)
    logger.info(f"HunyuanVideo-Foley service on http://127.0.0.1:{_args.port}")
    server.serve_forever()


if __name__ == "__main__":
    main()
