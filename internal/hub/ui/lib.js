// One import point for Preact, its hooks and htm (vendored, see vendor/VENDOR.md).
import { h, render, Fragment, createContext } from "./vendor/preact.module.js";
import {
  useState, useEffect, useLayoutEffect, useRef, useMemo, useCallback, useContext, useReducer,
} from "./vendor/hooks.module.js";
import htm from "./vendor/htm.module.js";

export const html = htm.bind(h);
export {
  h, render, Fragment, createContext,
  useState, useEffect, useLayoutEffect, useRef, useMemo, useCallback, useContext, useReducer,
};
