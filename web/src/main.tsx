import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app.tsx";
import "./styles.css";

const container = document.getElementById("root")!;
createRoot(container).render(<StrictMode><App /></StrictMode>);
