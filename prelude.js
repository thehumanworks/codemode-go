// Evaluated inside the QuickJS VM before the script. It is a function
// (namespace, toolsJson, globalsJson, hint) that builds the script's globals
// on top of four host functions the Go side installs beforehand:
//   __host_call(id, kind, name, argsJson)  start a host call, returns at once;
//                                          throws for unknown names, bad JSON
//                                          and the call limit
//   __host_wait()                          block until one call completes;
//                                          returns JSON {id, ok, payload, error}
//   __host_log(line)                       hand one console line to the host
//   __host_result(json)                    hand the return value to the host
// The prelude keeps them in a closure and removes them from globalThis, along
// with the quickjs-libc helpers the VM ships with, so a script reaches the
// host only through the functions defined here. The host does not rely on
// that alone: it validates what crosses the bridge.
(function (namespace, toolsJson, globalsJson, hint) {
  "use strict";
  const hostCall = globalThis.__host_call;
  const hostWait = globalThis.__host_wait;
  const hostLog = globalThis.__host_log;
  const hostResult = globalThis.__host_result;
  for (const name of [
    "__host_call", "__host_wait", "__host_log", "__host_result",
    // quickjs-libc: modules exposed as globals, print, timers, script arguments
    "std", "os", "bjson", "print", "setTimeout", "setInterval", "clearTimeout", "clearInterval", "scriptArgs", "gc", "QJS_PROXY_VALUE",
  ]) {
    delete globalThis[name];
  }
  // qjs's generated wrappers go through Function.prototype.call/apply; pin
  // them so a script cannot interpose on the bridge.
  Object.defineProperty(Function.prototype, "call", { writable: false, configurable: false });
  Object.defineProperty(Function.prototype, "apply", { writable: false, configurable: false });

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
      try {
        hostCall(id, kind, name, json);
      } catch (e) {
        reject(e instanceof Error ? e : new Error(String(e)));
        return;
      }
      pending.set(id, { resolve, reject });
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
  // host call is pending can never resume, since the sandbox has no timers.
  let driven = false;
  Object.defineProperty(globalThis, "__drive", {
    configurable: true,
    value: async function (main) {
      if (driven) throw new Error("__drive is not available to scripts");
      driven = true;
      delete globalThis.__drive;
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
          if (pending.size === 0) throw new Error("stalled: the script is waiting for a promise that nothing can settle (no tool call is pending, and the sandbox has no setTimeout or other timers)");
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
      let json;
      try {
        json = value === undefined ? undefined : stringify(value);
      } catch (e) {
        throw new Error("the return value cannot be serialised to JSON: " + (e && e.message ? e.message : e));
      }
      hostResult(json === undefined ? "null" : json);
      return true;
    },
  });
})
