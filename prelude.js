// Evaluated inside the QuickJS VM before the script. It is a function
// (namespace, toolsJson, globalsJson, hint) that builds the script's globals
// on top of three host functions the Go side installs beforehand:
//   __host_call(id, kind, name, argsJson)  start a host call, returns at once
//   __host_wait()                          block until one call completes;
//                                          returns JSON {id, ok, payload, error}
//   __host_log(line)                       hand one console line to the host
// The prelude keeps all three in a closure and removes them from globalThis,
// so a script reaches the host only through the functions defined here.
(function (namespace, toolsJson, globalsJson, hint) {
  "use strict";
  const hostCall = globalThis.__host_call;
  const hostWait = globalThis.__host_wait;
  const hostLog = globalThis.__host_log;
  delete globalThis.__host_call;
  delete globalThis.__host_wait;
  delete globalThis.__host_log;

  const stringify = JSON.stringify;
  const parse = JSON.parse;
  const pending = new Map();
  let nextId = 1;

  function call(kind, name, args) {
    return new Promise((resolve, reject) => {
      let json;
      try {
        json = args === undefined ? "null" : stringify(args);
        if (json === undefined) json = "null";
      } catch (e) {
        reject(e);
        return;
      }
      const id = nextId++;
      pending.set(id, { resolve, reject });
      hostCall(id, kind, name, json);
    });
  }

  // --- console ------------------------------------------------------------
  // Lines go straight to the host, which keeps them (and caps them), so they
  // survive a timeout or an out-of-memory abort.
  function format(v) {
    if (typeof v === "string") return v;
    if (v instanceof Error) return v.stack ? v.name + ": " + v.message + "\n" + v.stack : String(v);
    try {
      const s = stringify(v);
      return s === undefined ? String(v) : s;
    } catch {
      return String(v);
    }
  }
  function log(...args) {
    hostLog(args.map(format).join(" "));
  }
  globalThis.console = Object.freeze({ log, info: log, warn: log, error: log, debug: log });

  // --- tools namespace ----------------------------------------------------
  const comparable = (s) => String(s).toLowerCase().replace(/[^a-z0-9]/g, "");
  function guard(target, label, names) {
    return new Proxy(target, {
      get(obj, prop, recv) {
        if (typeof prop !== "string" || prop in obj || prop in Object.prototype || prop === "then" || prop === "toJSON") {
          return Reflect.get(obj, prop, recv);
        }
        const want = comparable(prop);
        const close = names.filter((n) => want && (comparable(n).includes(want) || want.includes(comparable(n))));
        let msg = label + "." + prop + " does not exist.";
        if (close.length) msg += " Did you mean " + close.slice(0, 5).map((n) => label + "." + n).join(", ") + "?";
        else if (names.length <= 24) msg += " Available: " + names.join(", ") + ".";
        if (hint) msg += " " + hint;
        throw new TypeError(msg);
      },
    });
  }
  const ns = Object.create(null);
  const shortNames = [];
  for (const { name, jsName } of parse(toolsJson)) {
    const fn = (args) => call("tool", name, args);
    if (!(jsName in ns)) {
      ns[jsName] = fn;
      shortNames.push(jsName);
    }
    if (!(name in ns)) ns[name] = fn;
  }
  Object.freeze(ns);
  Object.defineProperty(globalThis, namespace, {
    value: guard(ns, namespace, shortNames),
    enumerable: true,
  });

  // --- other globals (search, describe, ...) --------------------------------
  for (const name of parse(globalsJson)) {
    Object.defineProperty(globalThis, name, { value: (...args) => call("global", name, args), enumerable: true });
  }

  // --- driver -------------------------------------------------------------
  // Runs the script's promise to completion. Between host results it yields
  // to the microtask queue so continuations run; a script that waits while no
  // host call is pending can never resume, since the VM has no timers.
  globalThis.__drive = async function (main) {
    let settled = false, failed = false, value, error;
    main.then(
      (v) => { settled = true; value = v; },
      (e) => { settled = true; failed = true; error = e; },
    );
    await null;
    while (!settled) {
      if (pending.size === 0) {
        for (let i = 0; i < 100 && !settled && pending.size === 0; i++) await null;
        if (settled) break;
        if (pending.size === 0) throw new Error("stalled: the script awaits a promise that nothing can settle (no tool call is pending and the sandbox has no timers)");
      }
      const r = parse(hostWait());
      const p = pending.get(r.id);
      pending.delete(r.id);
      if (!p) continue;
      if (r.ok) p.resolve(r.payload === undefined || r.payload === null ? undefined : parse(r.payload));
      else p.reject(new Error(r.error));
      await null;
    }
    if (failed) throw error;
    // The host reads the result from this global as JSON. Reading the
    // eval's own return value is unreliable for short-lived strings.
    let json;
    try {
      json = value === undefined ? undefined : stringify(value);
    } catch (e) {
      throw new Error("the return value cannot be serialised to JSON: " + (e && e.message ? e.message : e));
    }
    globalThis.__result = json === undefined ? "null" : json;
    return true;
  };
})
