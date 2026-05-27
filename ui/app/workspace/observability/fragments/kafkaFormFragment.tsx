import { Button } from "@/components/ui/button";
import { EnvVarInput } from "@/components/ui/envVarInput";
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from "@/components/ui/form";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { kafkaFormSchema, type EnvVar, type KafkaFormSchema } from "@/lib/types/schemas";
import { toEnvVarFormValue } from "@/lib/utils/envVarForm";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { zodResolver } from "@hookform/resolvers/zod";
import { Plus, Trash2, X } from "lucide-react";
import { useEffect, useState } from "react";
import { useForm, type Resolver } from "react-hook-form";

interface KafkaSASLConfig {
	mechanism?: "plain" | "scram-sha-256" | "scram-sha-512";
	username?: string | EnvVar;
	password?: string | EnvVar;
}

interface KafkaTLSConfig {
	enabled?: boolean;
	skip_verify?: boolean;
	ca_cert?: string;
}

interface KafkaFormFragmentProps {
	currentConfig?: {
		enabled?: boolean;
		brokers?: string[];
		topic?: string;
		client_id?: string;
		sasl?: KafkaSASLConfig;
		tls?: KafkaTLSConfig;
		flush_frequency_ms?: number;
		max_buffer_size?: number;
	};
	onSave: (config: KafkaFormSchema) => Promise<void>;
	onDelete?: () => void;
	isDeleting?: boolean;
}

const buildDefaults = (cfg?: KafkaFormFragmentProps["currentConfig"]): KafkaFormSchema => ({
	enabled: cfg?.enabled ?? true,
	kafka_config: {
		brokers: cfg?.brokers && cfg.brokers.length > 0 ? cfg.brokers : [""],
		topic: cfg?.topic ?? "",
		client_id: cfg?.client_id ?? "",
		sasl_enabled: !!cfg?.sasl,
		sasl: {
			mechanism: cfg?.sasl?.mechanism ?? "plain",
			username: toEnvVarFormValue(cfg?.sasl?.username),
			password: toEnvVarFormValue(cfg?.sasl?.password),
		},
		tls_enabled: cfg?.tls?.enabled ?? false,
		tls: {
			enabled: cfg?.tls?.enabled ?? false,
			skip_verify: cfg?.tls?.skip_verify ?? false,
			ca_cert: cfg?.tls?.ca_cert ?? "",
		},
		flush_frequency_ms: cfg?.flush_frequency_ms ?? 100,
		max_buffer_size: cfg?.max_buffer_size ?? 10000,
	},
});

export function KafkaFormFragment({ currentConfig, onSave, onDelete, isDeleting = false }: KafkaFormFragmentProps) {
	const hasKafkaAccess = useRbac(RbacResource.Observability, RbacOperation.Update);
	const [isSaving, setIsSaving] = useState(false);

	const form = useForm<KafkaFormSchema, any, KafkaFormSchema>({
		resolver: zodResolver(kafkaFormSchema) as Resolver<KafkaFormSchema, any, KafkaFormSchema>,
		mode: "onChange",
		reValidateMode: "onChange",
		defaultValues: buildDefaults(currentConfig),
	});

	useEffect(() => {
		form.reset(buildDefaults(currentConfig));
	}, [currentConfig]);

	const saslEnabled = form.watch("kafka_config.sasl_enabled");
	const tlsEnabled = form.watch("kafka_config.tls_enabled");
	const brokers = form.watch("kafka_config.brokers");

	const handleSubmit = (data: KafkaFormSchema) => {
		setIsSaving(true);
		onSave(data).finally(() => setIsSaving(false));
	};

	const addBroker = () => {
		form.setValue("kafka_config.brokers", [...(brokers ?? []), ""]);
	};

	const removeBroker = (index: number) => {
		const updated = (brokers ?? []).filter((_, i) => i !== index);
		form.setValue("kafka_config.brokers", updated.length > 0 ? updated : [""]);
	};

	return (
		<Form {...form}>
			<form onSubmit={form.handleSubmit(handleSubmit)} className="flex flex-col gap-6">
				{/* Enable toggle */}
				<FormField
					control={form.control}
					name="enabled"
					render={({ field }) => (
						<FormItem className="flex flex-row items-center gap-3">
							<FormControl>
								<Switch
									checked={field.value}
									onCheckedChange={field.onChange}
									disabled={!hasKafkaAccess}
									data-testid="kafka-enabled-toggle"
								/>
							</FormControl>
							<div className="flex flex-col">
								<FormLabel className="text-sm font-medium">Enabled</FormLabel>
								<FormDescription className="text-xs">Stream completed request traces as JSON to Kafka in real-time</FormDescription>
							</div>
						</FormItem>
					)}
				/>

				{/* Brokers */}
				<div className="flex flex-col gap-2">
					<div className="flex items-center justify-between">
						<FormLabel className="text-sm">
							Brokers <span className="text-destructive">*</span>
						</FormLabel>
						<Button
							type="button"
							variant="ghost"
							size="sm"
							className="h-7 gap-1 text-xs"
							onClick={addBroker}
							disabled={!hasKafkaAccess}
							data-testid="kafka-add-broker-btn"
						>
							<Plus className="h-3 w-3" /> Add broker
						</Button>
					</div>
					<FormDescription className="text-xs">
						Kafka broker addresses in <code>host:port</code> format
					</FormDescription>
					<div className="flex flex-col gap-2">
						{(brokers ?? [""]).map((_, index) => (
							<FormField
								key={index}
								control={form.control}
								name={`kafka_config.brokers.${index}`}
								render={({ field }) => (
									<FormItem>
										<div className="flex gap-2">
											<FormControl>
												<Input
													{...field}
													placeholder="localhost:9092"
													disabled={!hasKafkaAccess}
													data-testid={`kafka-broker-input-${index}`}
												/>
											</FormControl>
											{(brokers ?? []).length > 1 && (
												<Button
													type="button"
													variant="ghost"
													size="icon"
													className="h-9 w-9 shrink-0"
													onClick={() => removeBroker(index)}
													disabled={!hasKafkaAccess}
												>
													<X className="h-4 w-4" />
												</Button>
											)}
										</div>
										<FormMessage />
									</FormItem>
								)}
							/>
						))}
					</div>
				</div>

				{/* Topic */}
				<FormField
					control={form.control}
					name="kafka_config.topic"
					render={({ field }) => (
						<FormItem>
							<FormLabel>
								Topic <span className="text-destructive">*</span>
							</FormLabel>
							<FormDescription className="text-xs">Kafka topic to publish trace records to</FormDescription>
							<FormControl>
								<Input {...field} placeholder="bifrost-traces" disabled={!hasKafkaAccess} data-testid="kafka-topic-input" />
							</FormControl>
							<FormMessage />
						</FormItem>
					)}
				/>

				{/* Client ID */}
				<FormField
					control={form.control}
					name="kafka_config.client_id"
					render={({ field }) => (
						<FormItem>
							<FormLabel>Client ID</FormLabel>
							<FormDescription className="text-xs">
								Optional identifier sent to Kafka brokers (default: bifrost-kafka-plugin)
							</FormDescription>
							<FormControl>
								<Input
									{...field}
									value={field.value ?? ""}
									placeholder="bifrost-kafka-plugin"
									disabled={!hasKafkaAccess}
									data-testid="kafka-client-id-input"
								/>
							</FormControl>
							<FormMessage />
						</FormItem>
					)}
				/>

				{/* SASL */}
				<div className="flex flex-col gap-4 rounded-md border p-4">
					<FormField
						control={form.control}
						name="kafka_config.sasl_enabled"
						render={({ field }) => (
							<FormItem className="flex flex-row items-center gap-3">
								<FormControl>
									<Switch
										checked={field.value}
										onCheckedChange={field.onChange}
										disabled={!hasKafkaAccess}
										data-testid="kafka-sasl-enabled-toggle"
									/>
								</FormControl>
								<div className="flex flex-col">
									<FormLabel className="text-sm font-medium">SASL Authentication</FormLabel>
									<FormDescription className="text-xs">Enable SASL for broker authentication</FormDescription>
								</div>
							</FormItem>
						)}
					/>

					{saslEnabled && (
						<div className="flex flex-col gap-4">
							<FormField
								control={form.control}
								name="kafka_config.sasl.mechanism"
								render={({ field }) => (
									<FormItem>
										<FormLabel>Mechanism</FormLabel>
										<Select onValueChange={field.onChange} value={field.value} disabled={!hasKafkaAccess}>
											<FormControl>
												<SelectTrigger data-testid="kafka-sasl-mechanism-select">
													<SelectValue placeholder="Select mechanism" />
												</SelectTrigger>
											</FormControl>
											<SelectContent>
												<SelectItem value="plain">PLAIN</SelectItem>
												<SelectItem value="scram-sha-256">SCRAM-SHA-256</SelectItem>
												<SelectItem value="scram-sha-512">SCRAM-SHA-512</SelectItem>
											</SelectContent>
										</Select>
										<FormMessage />
									</FormItem>
								)}
							/>

							<FormField
								control={form.control}
								name="kafka_config.sasl.username"
								render={({ field }) => (
									<FormItem>
										<FormLabel>Username</FormLabel>
										<FormControl>
											<EnvVarInput
												value={field.value}
												onChange={field.onChange}
												placeholder="SASL username"
												disabled={!hasKafkaAccess}
												data-testid="kafka-sasl-username-input"
											/>
										</FormControl>
										<FormMessage />
									</FormItem>
								)}
							/>

							<FormField
								control={form.control}
								name="kafka_config.sasl.password"
								render={({ field }) => (
									<FormItem>
										<FormLabel>Password</FormLabel>
										<FormControl>
											<EnvVarInput
												value={field.value}
												onChange={field.onChange}
												placeholder="SASL password"
												maskNonEnvValue
												disabled={!hasKafkaAccess}
												data-testid="kafka-sasl-password-input"
											/>
										</FormControl>
										<FormMessage />
									</FormItem>
								)}
							/>
						</div>
					)}
				</div>

				{/* TLS */}
				<div className="flex flex-col gap-4 rounded-md border p-4">
					<FormField
						control={form.control}
						name="kafka_config.tls_enabled"
						render={({ field }) => (
							<FormItem className="flex flex-row items-center gap-3">
								<FormControl>
									<Switch
										checked={field.value}
										onCheckedChange={(v) => {
											field.onChange(v);
											form.setValue("kafka_config.tls.enabled", v);
										}}
										disabled={!hasKafkaAccess}
										data-testid="kafka-tls-enabled-toggle"
									/>
								</FormControl>
								<div className="flex flex-col">
									<FormLabel className="text-sm font-medium">TLS</FormLabel>
									<FormDescription className="text-xs">Enable TLS encryption for broker connections</FormDescription>
								</div>
							</FormItem>
						)}
					/>

					{tlsEnabled && (
						<div className="flex flex-col gap-4">
							<FormField
								control={form.control}
								name="kafka_config.tls.skip_verify"
								render={({ field }) => (
									<FormItem className="flex flex-row items-center gap-3">
										<FormControl>
											<Switch
												checked={field.value}
												onCheckedChange={field.onChange}
												disabled={!hasKafkaAccess}
												data-testid="kafka-tls-skip-verify-toggle"
											/>
										</FormControl>
										<div className="flex flex-col">
											<FormLabel className="text-sm">Insecure (Skip TLS Verification)</FormLabel>
											<FormDescription className="text-xs text-yellow-600 dark:text-yellow-400">
												Skip TLS certificate verification. Disable to use system root CAs or a custom CA.
											</FormDescription>
										</div>
									</FormItem>
								)}
							/>

							<FormField
								control={form.control}
								name="kafka_config.tls.ca_cert"
								render={({ field }) => (
									<FormItem>
										<FormLabel>CA Certificate</FormLabel>
										<FormDescription className="text-xs">PEM-encoded CA certificate for broker TLS verification (optional)</FormDescription>
										<FormControl>
											<textarea
												{...field}
												value={field.value ?? ""}
												rows={4}
												placeholder={"-----BEGIN CERTIFICATE-----\n..."}
												disabled={!hasKafkaAccess}
												data-testid="kafka-tls-ca-cert-input"
												className="border-input bg-background text-foreground placeholder:text-muted-foreground focus-visible:ring-ring flex min-h-[80px] w-full rounded-md border px-3 py-2 text-sm focus-visible:ring-1 focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
											/>
										</FormControl>
										<FormMessage />
									</FormItem>
								)}
							/>
						</div>
					)}
				</div>

				{/* Advanced settings */}
				<div className="grid grid-cols-2 gap-4">
					<FormField
						control={form.control}
						name="kafka_config.flush_frequency_ms"
						render={({ field }) => (
							<FormItem>
								<FormLabel>Flush Frequency (ms)</FormLabel>
								<FormDescription className="text-xs">How often buffered records are flushed to Kafka</FormDescription>
								<FormControl>
									<Input
										type="number"
										min={1}
										max={10000}
										{...field}
										value={field.value}
										onChange={(e) => field.onChange(Number(e.target.value))}
										disabled={!hasKafkaAccess}
										data-testid="kafka-flush-frequency-input"
									/>
								</FormControl>
								<FormMessage />
							</FormItem>
						)}
					/>

					<FormField
						control={form.control}
						name="kafka_config.max_buffer_size"
						render={({ field }) => (
							<FormItem>
								<FormLabel>Max Buffer Size</FormLabel>
								<FormDescription className="text-xs">Max records in memory before oldest are dropped</FormDescription>
								<FormControl>
									<Input
										type="number"
										min={1}
										{...field}
										value={field.value}
										onChange={(e) => field.onChange(Number(e.target.value))}
										disabled={!hasKafkaAccess}
										data-testid="kafka-max-buffer-size-input"
									/>
								</FormControl>
								<FormMessage />
							</FormItem>
						)}
					/>
				</div>

				{/* Actions */}
				<div className="flex items-center justify-between pt-2">
					{onDelete && (
						<TooltipProvider>
							<Tooltip>
								<TooltipTrigger asChild>
									<Button
										type="button"
										variant="destructive"
										size="sm"
										onClick={onDelete}
										disabled={isDeleting || !hasKafkaAccess}
										data-testid="kafka-delete-btn"
									>
										<Trash2 className="mr-1.5 h-4 w-4" />
										{isDeleting ? "Removing…" : "Remove"}
									</Button>
								</TooltipTrigger>
								<TooltipContent>Remove Kafka connector</TooltipContent>
							</Tooltip>
						</TooltipProvider>
					)}
					<Button type="submit" size="sm" disabled={isSaving || !hasKafkaAccess} className="ml-auto" data-testid="kafka-save-btn">
						{isSaving ? "Saving…" : "Save Kafka Configuration"}
					</Button>
				</div>
			</form>
		</Form>
	);
}