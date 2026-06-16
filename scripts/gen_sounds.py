#!/usr/bin/env python3
"""
Generates synthesized WAV sound effects for Splitclicker.
Run from the repo root: python scripts/gen_sounds.py
Requires: numpy

Every voice is a square + triangle wave of the same note overlapped (the triangle
slightly detuned for a little shimmer), shaped by a per-sound envelope. Four sounds,
tied together as a tiny musical motif around G:

  arming   - "G" beeped twice (50 ms each, 150 ms apart) while the button is dormant
  armed    - "C" above that G, 150 ms, fading out, as the button goes live
  click    - "D" above the armed C, a short snappy blip on each scoring click
  disarm   - "C" an octave below the G, double-beeped like arming, on round/game over
  throttle - a short, dull, downward buzz when the player clicks while NOT armed
             (the idle-click spam deterrent — "nope", in every state but CLICK!)

Notes (equal temperament):
  G4 392.00, C5 523.25, D5 587.33, C4 261.63
"""
import math
import os
import wave

import numpy as np

SR = 44100

G4 = 392.00   # arming
C5 = 523.25   # armed   (C above G)
D5 = 587.33   # click   (D above the armed C)
C4 = 261.63   # disarm  (C below G)


def write_wav(path, samples):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    pcm = np.clip(samples * 32767, -32767, 32767).astype(np.int16)
    with wave.open(path, "w") as f:
        f.setnchannels(1)
        f.setsampwidth(2)
        f.setframerate(SR)
        f.writeframes(pcm.tobytes())
    print(f"  {path}  ({len(samples) / SR * 1000:.0f} ms)")


def normalize(samples, peak=0.6):
    m = np.abs(samples).max()
    return samples * (peak / m) if m > 0 else samples


def blend(freq, n, sq=0.45, tri=0.55, detune=1.004):
    """The house voice: a square + a slightly-detuned triangle at `freq`, n samples."""
    t = np.arange(n) / SR
    square = np.sign(np.sin(2 * math.pi * freq * t))
    triangle = (2 / math.pi) * np.arcsin(np.sin(2 * math.pi * freq * detune * t))
    return sq * square + tri * triangle


def ar_env(n, attack, release):
    """Flat-topped envelope with linear attack/release (ms-scale) to kill edge clicks."""
    env = np.ones(n)
    a, r = min(int(attack * SR), n), min(int(release * SR), n)
    if a > 0:
        env[:a] *= np.linspace(0, 1, a)
    if r > 0:
        env[-r:] *= np.linspace(1, 0, r)
    return env


def beep(freq, dur):
    n = int(dur * SR)
    return blend(freq, n) * ar_env(n, 0.006, 0.012)


def double_beep(freq, beep_dur=0.050, gap=0.150):
    """Two short beeps `gap` seconds apart — the arming/disarm rhythm."""
    b = beep(freq, beep_dur)
    silence = np.zeros(int(gap * SR))
    return np.concatenate([b, silence, b])


def gen_arming():
    # "G" double beep — the dormant/waiting heartbeat.
    return normalize(double_beep(G4))


def gen_armed():
    # "C" above the G, 150 ms, quick attack then exponential fade-out. A touch of the
    # octave above is mixed in for a brighter "go!" ring (the flair).
    dur = 0.150
    n = int(dur * SR)
    t = np.arange(n) / SR
    osc = blend(C5, n) + 0.18 * np.sin(2 * math.pi * C5 * 2 * t)
    attack = int(0.006 * SR)
    env = np.empty(n)
    env[:attack] = np.linspace(0, 1, attack)
    env[attack:] = np.exp(np.linspace(0, math.log(0.02), n - attack))
    return normalize(osc * env)


def gen_click():
    # "D" above the armed C, short and punchy with a tiny noise transient on the
    # attack so a fast click feels snappy. Fast exponential decay.
    dur = 0.090
    n = int(dur * SR)
    osc = blend(D5, n)
    trans = int(0.004 * SR)
    osc[:trans] += np.random.uniform(-1, 1, trans) * np.linspace(0.8, 0, trans)
    attack = int(0.002 * SR)
    env = np.empty(n)
    env[:attack] = np.linspace(0, 1, attack)
    env[attack:] = np.exp(np.linspace(0, math.log(0.01), n - attack))
    return normalize(osc * env)


def gen_disarm():
    # "C" an octave below the G, double-beeped like arming — the round/game bookend.
    return normalize(double_beep(C4))


def gen_throttle():
    # The "nope" — fired when the player mashes while the button is dormant. A low,
    # dull buzz that bends downward (G3 → roughly a tritone below) so it reads as
    # rejection, not reward. Square-heavy for grit; lowpassed so it stays muffled.
    dur = 0.070
    n = int(dur * SR)
    freq = np.exp(np.linspace(math.log(196.0), math.log(138.0), n))  # G3 → ~C#3
    phase = np.cumsum(freq / SR)
    square = np.sign(np.sin(2 * math.pi * phase))
    triangle = (2 / math.pi) * np.arcsin(np.sin(2 * math.pi * phase * 1.004))
    osc = 0.65 * square + 0.35 * triangle
    # Single-pole lowpass at 700 Hz to take the fizz off the square edges.
    rc = 1.0 / (2 * math.pi * 700)
    alpha = (1.0 / SR) / (rc + 1.0 / SR)
    filt = np.zeros(n)
    filt[0] = osc[0] * alpha
    for i in range(1, n):
        filt[i] = filt[i - 1] + alpha * (osc[i] - filt[i - 1])
    return normalize(filt * ar_env(n, 0.004, 0.020), peak=0.5)


OUT = "client/Assets/sounds/sfx"
print("Generating sound effects…")
write_wav(f"{OUT}/arming.wav", gen_arming())
write_wav(f"{OUT}/armed.wav", gen_armed())
write_wav(f"{OUT}/click.wav", gen_click())
write_wav(f"{OUT}/disarm.wav", gen_disarm())
write_wav(f"{OUT}/throttle.wav", gen_throttle())
print("Done.")
