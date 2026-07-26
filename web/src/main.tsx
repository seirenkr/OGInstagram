import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Root } from "./app.tsx";
import "./styles.css";

const container = document.getElementById("root")!;
createRoot(container).render(<StrictMode><Root /></StrictMode>);
